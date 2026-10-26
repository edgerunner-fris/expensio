package expense

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func actorFor(userID uuid.UUID, role tenantctx.Role) Actor {
	return Actor{UserID: userID, Role: role}
}

// createBudgetFor 通过 service 建预算，保证走的是与线上相同的校验路径。
func createBudgetFor(t *testing.T, svc *Service, ctx context.Context, who Actor, category string, amount int64) *Budget {
	t.Helper()
	in := newBudgetInput()
	in.Category = category
	in.AmountMinor = amount
	b, err := svc.CreateBudget(ctx, who, in)
	require.NoError(t, err)
	return b
}

func budgetAfter(t *testing.T, svc *Service, ctx context.Context, who Actor, id uuid.UUID) *Budget {
	t.Helper()
	b, err := svc.GetBudget(ctx, who, id)
	require.NoError(t, err)
	return b
}

// approvalTrail 读出审批轨迹，格式为 "action:from>to"。
// 只读查询也必须走租户上下文，否则 RLS 返回零行。
func approvalTrail(t *testing.T, database *db.DB, ctx context.Context, expenseID uuid.UUID) []string {
	t.Helper()
	var out []string
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT action || ':' || from_status || '>' || to_status
			FROM app.approval_steps
			WHERE expense_id = $1
			ORDER BY created_at, id`, expenseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}))
	return out
}

func TestWorkflowSubmitApproveReimburse(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	b := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 100_000)
	e, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	require.Nil(t, e.BudgetID, "草稿阶段还没有匹配预算")

	submitted, err := svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	assert.Equal(t, StatusSubmitted, submitted.Status)
	require.NotNil(t, submitted.BudgetID)
	assert.Equal(t, b.ID, *submitted.BudgetID, "提交时自动匹配到唯一预算")
	assert.Equal(t, 2, submitted.Version)

	afterSubmit := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 12_345, afterSubmit.ReservedMinor)
	assert.EqualValues(t, 0, afterSubmit.SpentMinor)
	assert.EqualValues(t, 87_655, afterSubmit.AvailableMinor())

	approved, err := svc.ApplyAction(adminCtx, adminWho, e.ID, ActionApprove, "同意")
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, approved.Status)
	require.NotNil(t, approved.DecidedBy)
	assert.Equal(t, admin, *approved.DecidedBy)
	require.NotNil(t, approved.DecidedAt)
	assert.Equal(t, 3, approved.Version)

	afterApprove := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 0, afterApprove.ReservedMinor, "审批通过后预占清零")
	assert.EqualValues(t, 12_345, afterApprove.SpentMinor, "预占转为支出")

	reimbursed, err := svc.ApplyAction(adminCtx, adminWho, e.ID, ActionReimburse, "已打款")
	require.NoError(t, err)
	assert.Equal(t, StatusReimbursed, reimbursed.Status)
	require.NotNil(t, reimbursed.DecidedBy)
	assert.Equal(t, admin, *reimbursed.DecidedBy, "报销不覆盖审批人")

	afterReimburse := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 0, afterReimburse.ReservedMinor)
	assert.EqualValues(t, 12_345, afterReimburse.SpentMinor, "报销不重复占用预算")

	assert.Equal(t, []string{
		"submit:draft>submitted",
		"approve:submitted>approved",
		"reimburse:approved>reimbursed",
	}, approvalTrail(t, database, memberCtx, e.ID))
}

func TestWorkflowWithoutBudgets(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	// 租户从未配置预算：允许无预算提交（已确认规则 A 的另一半）。
	e, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	submitted, err := svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	assert.Nil(t, submitted.BudgetID)

	approved, err := svc.ApplyAction(adminCtx, adminWho, e.ID, ActionApprove, "")
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, approved.Status)
	assert.Equal(t, 2, len(approvalTrail(t, database, memberCtx, e.ID)))
}

func TestWorkflowBudgetUnavailable(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	b := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 10_000)
	e, err := svc.CreateDraft(memberCtx, memberWho, validInput()) // 12_345
	require.NoError(t, err)

	_, err = svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
	requireAppErr(t, err, CodeBudgetUnavailable, 409)

	fresh, err := svc.Get(memberCtx, memberWho, e.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusDraft, fresh.Status, "预占失败必须整体回滚")
	assert.Equal(t, 1, fresh.Version)
	assert.Nil(t, fresh.BudgetID)

	budget := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 0, budget.ReservedMinor)
	assert.Empty(t, approvalTrail(t, database, memberCtx, e.ID), "失败的动作不能留下轨迹")
}

func TestWorkflowBudgetNotMatched(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	meals := createBudgetFor(t, svc, adminCtx, adminWho, "meals", 100_000)

	// 租户已有预算，但类别不匹配 → 不能静默改成无预算提交。
	e, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
	requireAppErr(t, err, CodeBudgetNotMatched, 409)

	// 显式指定了不匹配的预算，同样拒绝。
	in := validInput()
	in.BudgetID = &meals.ID
	e2, err := svc.CreateDraft(memberCtx, memberWho, in)
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, e2.ID, ActionSubmit, "")
	requireAppErr(t, err, CodeBudgetNotMatched, 409)

	assert.EqualValues(t, 0, budgetAfter(t, svc, adminCtx, adminWho, meals.ID).ReservedMinor)
}

func TestWorkflowSelfApprovalForbidden(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, owner := uuid.New(), uuid.New()
	ownerCtx := tenantCtx(t, tenant, owner, tenantctx.RoleOwner)
	ownerWho := actorFor(owner, tenantctx.RoleOwner)

	b := createBudgetFor(t, svc, ownerCtx, ownerWho, "travel", 100_000)
	e, err := svc.CreateDraft(ownerCtx, ownerWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(ownerCtx, ownerWho, e.ID, ActionSubmit, "")
	require.NoError(t, err)

	// 所有者也不能审批自己的单。
	_, err = svc.ApplyAction(ownerCtx, ownerWho, e.ID, ActionApprove, "")
	requireAppErr(t, err, CodeSelfApproval, 403)

	fresh, err := svc.Get(ownerCtx, ownerWho, e.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSubmitted, fresh.Status)
	assert.EqualValues(t, 12_345, budgetAfter(t, svc, ownerCtx, ownerWho, b.ID).ReservedMinor,
		"被拒绝的审批不能动预算")
}

func TestWorkflowRejectAndCancelReleaseBudget(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	b := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 100_000)

	rejected, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, rejected.ID, ActionSubmit, "")
	require.NoError(t, err)

	// 驳回必须写理由。
	_, err = svc.ApplyAction(adminCtx, adminWho, rejected.ID, ActionReject, "   ")
	require.Error(t, err)
	assert.Equal(t, 400, apperr.Status(err))
	_, err = svc.ApplyAction(adminCtx, adminWho, rejected.ID, ActionReject, strings.Repeat("a", maxCommentRunes+1))
	require.Error(t, err)
	assert.Equal(t, 400, apperr.Status(err))

	got, err := svc.ApplyAction(adminCtx, adminWho, rejected.ID, ActionReject, "发票缺失")
	require.NoError(t, err)
	assert.Equal(t, StatusRejected, got.Status)
	assert.Equal(t, "发票缺失", got.RejectionReason)
	assert.EqualValues(t, 0, budgetAfter(t, svc, adminCtx, adminWho, b.ID).ReservedMinor)

	cancelled, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, cancelled.ID, ActionSubmit, "")
	require.NoError(t, err)
	assert.EqualValues(t, 12_345, budgetAfter(t, svc, adminCtx, adminWho, b.ID).ReservedMinor)

	got, err = svc.ApplyAction(memberCtx, memberWho, cancelled.ID, ActionCancel, "")
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, got.Status)
	assert.Nil(t, got.DecidedBy, "撤销不是审批，不写审批人")
	after := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 0, after.ReservedMinor)
	assert.EqualValues(t, 0, after.SpentMinor)
}

func TestWorkflowInvalidTransitions(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	draft, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)

	_, err = svc.ApplyAction(adminCtx, adminWho, draft.ID, ActionApprove, "")
	requireAppErr(t, err, CodeStateConflict, 409)

	_, err = svc.ApplyAction(memberCtx, memberWho, draft.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, draft.ID, ActionSubmit, "")
	requireAppErr(t, err, CodeStateConflict, 409)

	_, err = svc.ApplyAction(adminCtx, adminWho, draft.ID, ActionReimburse, "")
	requireAppErr(t, err, CodeStateConflict, 409)

	_, err = svc.ApplyAction(adminCtx, adminWho, draft.ID, Action("explode"), "")
	require.Error(t, err)
	assert.Equal(t, 400, apperr.Status(err))

	// 已批准的单不能撤销。
	_, err = svc.ApplyAction(adminCtx, adminWho, draft.ID, ActionApprove, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(adminCtx, adminWho, draft.ID, ActionCancel, "")
	requireAppErr(t, err, CodeStateConflict, 409)

	// 被降级为 viewer 的成员：草稿还在，但不能再提交。
	viewer := uuid.New()
	memberRoleCtx := tenantCtx(t, tenant, viewer, tenantctx.RoleMember)
	viewerDraft, err := svc.CreateDraft(memberRoleCtx, actorFor(viewer, tenantctx.RoleMember), validInput())
	require.NoError(t, err)
	viewerCtx := tenantCtx(t, tenant, viewer, tenantctx.RoleViewer)
	_, err = svc.ApplyAction(viewerCtx, actorFor(viewer, tenantctx.RoleViewer), viewerDraft.ID, ActionSubmit, "")
	require.Error(t, err)
	assert.Equal(t, 403, apperr.Status(err))
}

func TestConcurrentSubmitRespectsBudget(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin := uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	adminWho := actorFor(admin, tenantctx.RoleAdmin)

	// 预算只够一张单。
	b := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 12_345)

	const n = 2
	ctxs := make([]context.Context, n)
	whos := make([]Actor, n)
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		user := uuid.New()
		ctxs[i] = tenantCtx(t, tenant, user, tenantctx.RoleMember)
		whos[i] = actorFor(user, tenantctx.RoleMember)
		e, err := svc.CreateDraft(ctxs[i], whos[i], validInput())
		require.NoError(t, err)
		ids[i] = e.ID
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.ApplyAction(ctxs[i], whos[i], ids[i], ActionSubmit, "")
		}(i)
	}
	wg.Wait()

	success := 0
	for _, err := range errs {
		if err == nil {
			success++
			continue
		}
		requireAppErr(t, err, CodeBudgetUnavailable, 409)
	}
	assert.Equal(t, 1, success, "条件更新必须只放行一个提交")

	budget := budgetAfter(t, svc, adminCtx, adminWho, b.ID)
	assert.EqualValues(t, 12_345, budget.ReservedMinor, "预占不能超过预算额度")

	total := 0
	for _, id := range ids {
		total += len(approvalTrail(t, database, adminCtx, id))
	}
	assert.Equal(t, 1, total, "失败的提交不能留下审批轨迹")
}
