package expense

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestMultiLevelApproval(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)

	tenant, submitter := uuid.New(), uuid.New()
	first, second := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, submitter, tenantctx.RoleOwner)
	submitterActor := actorFor(submitter, tenantctx.RoleOwner)
	firstApprover := actorFor(first, tenantctx.RoleAdmin)
	secondApprover := actorFor(second, tenantctx.RoleAdmin)

	budget := createBudgetFor(t, svc, ctx, submitterActor, "travel", 100_000)

	// USD 达到 500 美元（50000 分）需要两级审批。
	_, err := svc.UpsertPolicy(ctx, submitterActor, "usd", PolicyInput{SecondLevelThresholdMinor: 50_000})
	require.NoError(t, err)

	in := validInput()
	in.AmountMinor = 60_000
	in.BudgetID = &budget.ID
	e, err := svc.CreateDraft(ctx, submitterActor, in)
	require.NoError(t, err)

	submitted, err := svc.ApplyAction(ctx, submitterActor, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	assert.Equal(t, int16(2), submitted.RequiredApprovalLevels, "提交时按金额快照级数")
	assert.Equal(t, int16(0), submitted.ApprovalLevel)

	assert.EqualValues(t, 60_000, budgetAfter(t, svc, ctx, submitterActor, budget.ID).ReservedMinor)
	assert.Zero(t, budgetAfter(t, svc, ctx, submitterActor, budget.ID).SpentMinor,
		"只有最后一级批准才结转支出")

	// 第一级：状态仍是 submitted，只推进会签计数。
	afterFirst, err := svc.ApplyAction(ctx, firstApprover, e.ID, ActionApprove, "一级同意")
	require.NoError(t, err)
	assert.Equal(t, StatusSubmitted, afterFirst.Status)
	assert.Equal(t, int16(1), afterFirst.ApprovalLevel)
	assert.Equal(t, 0, countRows(t, database, ctx,
		`SELECT count(*) FROM app.approval_steps WHERE expense_id = $1 AND action = 'approve'`, e.ID),
		"中间级不写状态轨迹（那份 CHECK 要求 submitted→approved）")

	assert.EqualValues(t, 60_000, budgetAfter(t, svc, ctx, submitterActor, budget.ID).ReservedMinor)
	assert.Zero(t, budgetAfter(t, svc, ctx, submitterActor, budget.ID).SpentMinor)

	// 同一人不能批两级：数据库唯一约束是最后防线，服务层翻译成 409。
	_, err = svc.ApplyAction(ctx, firstApprover, e.ID, ActionApprove, "再批一次")
	requireAppErr(t, err, CodeDuplicateApprover, 409)

	// 第二级由另一人完成，单据才算批准。
	final, err := svc.ApplyAction(ctx, secondApprover, e.ID, ActionApprove, "二级同意")
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, final.Status)
	assert.Equal(t, int16(2), final.ApprovalLevel)
	assert.Equal(t, 1, countRows(t, database, ctx,
		`SELECT count(*) FROM app.approval_steps WHERE expense_id = $1 AND action = 'approve'`, e.ID))

	after := budgetAfter(t, svc, ctx, submitterActor, budget.ID)
	assert.Zero(t, after.ReservedMinor, "批准后预占应转为支出")
	assert.EqualValues(t, 60_000, after.SpentMinor)

	records, got, err := svc.ListApprovals(ctx, secondApprover, e.ID)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, first, records[0].ActorID)
	assert.Equal(t, second, records[1].ActorID)
	assert.Equal(t, int16(2), got.RequiredApprovalLevels)

	// 事件：submit 一条；两次 approve 中只有最后一级一条。
	evs := outboxForExpense(t, database, tenant, e.ID)
	require.Len(t, evs, 2)
	assert.Equal(t, string(events.TypeExpenseSubmitted), evs[0].Type)
	assert.Equal(t, string(events.TypeExpenseApproved), evs[1].Type)
	assert.Equal(t, "approved", decodeSnapshot(t, evs[1].Payload).Status)
}

func TestApprovalPolicySnapshotAndInbox(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)

	tenant, submitter := uuid.New(), uuid.New()
	approver := uuid.New()
	ctx := tenantCtx(t, tenant, submitter, tenantctx.RoleOwner)
	submitterActor := actorFor(submitter, tenantctx.RoleOwner)
	approverActor := actorFor(approver, tenantctx.RoleAdmin)

	_, err := svc.UpsertPolicy(ctx, submitterActor, "USD", PolicyInput{SecondLevelThresholdMinor: 10_000})
	require.NoError(t, err)

	in := validInput()
	in.AmountMinor = 20_000
	e, err := svc.CreateDraft(ctx, submitterActor, in)
	require.NoError(t, err)
	_, err = svc.ApplyAction(ctx, submitterActor, e.ID, ActionSubmit, "")
	require.NoError(t, err)

	// 提交后调高门槛：在途单据的快照不变。
	_, err = svc.UpsertPolicy(ctx, submitterActor, "USD", PolicyInput{SecondLevelThresholdMinor: 1_000_000})
	require.NoError(t, err)
	got, err := svc.Get(ctx, submitterActor, e.ID)
	require.NoError(t, err)
	assert.Equal(t, int16(2), got.RequiredApprovalLevels, "策略变更不影响在途单据")

	// 新单据按新门槛走一级审批。
	in2 := validInput()
	in2.AmountMinor = 20_000
	e2, err := svc.CreateDraft(ctx, submitterActor, in2)
	require.NoError(t, err)
	got2, err := svc.ApplyAction(ctx, submitterActor, e2.ID, ActionSubmit, "")
	require.NoError(t, err)
	assert.Equal(t, int16(1), got2.RequiredApprovalLevels)

	// 待办队列：提交的单据出现在未批过的管理员队列里，批过之后消失。
	items, total, err := svc.List(ctx, approverActor, ListQuery{Inbox: true, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Len(t, items, 2)

	_, err = svc.ApplyAction(ctx, approverActor, e2.ID, ActionApprove, "同意")
	require.NoError(t, err)

	items, total, err = svc.List(ctx, approverActor, ListQuery{Inbox: true, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "已批准过的单据不再出现在自己的待办里")
	assert.Equal(t, e.ID, items[0].ID)

	// 普通成员没有待办队列。
	memberActor := actorFor(uuid.New(), tenantctx.RoleMember)
	memberCtx := tenantCtx(t, tenant, memberActor.UserID, tenantctx.RoleMember)
	_, _, err = svc.List(memberCtx, memberActor, ListQuery{Inbox: true, Limit: 10})
	assert.Equal(t, 403, apperr.Status(err))

	// 非管理岗不能改策略。
	_, err = svc.UpsertPolicy(memberCtx, memberActor, "USD", PolicyInput{SecondLevelThresholdMinor: 1})
	assert.Equal(t, 403, apperr.Status(err))

	// 删除策略后回到单级审批。
	require.NoError(t, svc.DeletePolicy(ctx, submitterActor, "USD"))
	policies, err := svc.ListPolicies(ctx, submitterActor)
	require.NoError(t, err)
	assert.Empty(t, policies)
}

func TestApprovalRejectsSelfApprovalAndBadLevels(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)

	tenant, submitter := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, submitter, tenantctx.RoleOwner)
	submitterActor := actorFor(submitter, tenantctx.RoleOwner)

	_, err := svc.UpsertPolicy(ctx, submitterActor, "USD", PolicyInput{SecondLevelThresholdMinor: 1})
	require.NoError(t, err)

	in := validInput()
	in.AmountMinor = 100
	e, err := svc.CreateDraft(ctx, submitterActor, in)
	require.NoError(t, err)
	_, err = svc.ApplyAction(ctx, submitterActor, e.ID, ActionSubmit, "")
	require.NoError(t, err)

	// 自审在领域层就被挡住。
	_, err = svc.ApplyAction(ctx, submitterActor, e.ID, ActionApprove, "自己批")
	requireAppErr(t, err, CodeSelfApproval, 403)

	// 跳过第一级直接插第二级会被数据库触发器拒绝：这里用 SQL 绕过服务层验证。
	err = execInTenant(t, database, ctx, `
		INSERT INTO app.expense_approvals (id, tenant_id, expense_id, level, actor_id)
		VALUES ($1, $2, $3, 2, $4)`, uuid.New(), tenant, e.ID, uuid.New())
	require.Error(t, err)
	assert.Equal(t, "23514", sqlState(err))
}
