package expense

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

type outboxEvent struct {
	ID      uuid.UUID
	Type    string
	Version int
	Payload []byte
}

// outboxForExpense 直接读 outbox：该表刻意没有 RLS，测试用 app 角色即可，
// 但查询显式带上 tenant_id 与 aggregate_id，避免读到其他用例的数据。
func outboxForExpense(t *testing.T, database *db.DB, tenantID, expenseID uuid.UUID) []outboxEvent {
	t.Helper()
	var out []outboxEvent
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, event_type, aggregate_version, payload
			FROM app.outbox_events
			WHERE tenant_id = $1 AND aggregate_id = $2
			ORDER BY aggregate_version`, tenantID, expenseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e outboxEvent
			if err := rows.Scan(&e.ID, &e.Type, &e.Version, &e.Payload); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	}))
	return out
}

func decodeSnapshot(t *testing.T, raw []byte) events.ExpenseSnapshot {
	t.Helper()
	// outbox 的 payload 列存的是完整信封，快照在信封的 payload 字段里。
	env, err := events.Decode(raw)
	require.NoError(t, err)
	var snap events.ExpenseSnapshot
	require.NoError(t, json.Unmarshal(env.Payload, &snap))
	require.NoError(t, snap.Validate())
	return snap
}

func TestWorkflowActionsWriteOutbox(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

	budget := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 100_000)
	e, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)

	_, err = svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(adminCtx, adminWho, e.ID, ActionApprove, "同意")
	require.NoError(t, err)
	_, err = svc.ApplyAction(adminCtx, adminWho, e.ID, ActionReimburse, "已打款")
	require.NoError(t, err)

	got := outboxForExpense(t, database, tenant, e.ID)
	require.Len(t, got, 3)
	assert.Equal(t, []string{
		string(events.TypeExpenseSubmitted),
		string(events.TypeExpenseApproved),
		string(events.TypeExpenseReimbursed),
	}, []string{got[0].Type, got[1].Type, got[2].Type})
	assert.Equal(t, []int{2, 3, 4}, []int{got[0].Version, got[1].Version, got[2].Version})

	wantStatus := []string{"submitted", "approved", "reimbursed"}
	for i, row := range got {
		snap := decodeSnapshot(t, row.Payload)
		assert.Equal(t, e.ID, snap.ID)
		assert.Equal(t, tenant, snap.TenantID, "事件租户必须来自数据库记录")
		assert.Equal(t, member, snap.SubmitterID)
		assert.Equal(t, wantStatus[i], snap.Status)
		assert.Equal(t, row.Version, snap.Version, "信封版本与快照版本必须一致")
		assert.EqualValues(t, 12_345, snap.AmountMinor)
		assert.Equal(t, "USD", snap.Currency)
		if i == 0 {
			require.NotNil(t, snap.BudgetID)
			assert.Equal(t, budget.ID, *snap.BudgetID)
		}
	}
	require.NotNil(t, decodeSnapshot(t, got[1].Payload).DecidedBy)
	assert.Equal(t, admin, *decodeSnapshot(t, got[1].Payload).DecidedBy)
}

func TestRejectAndCancelWriteOutboxWithoutSensitiveText(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)
	createBudgetFor(t, svc, adminCtx, adminWho, "travel", 100_000)

	rejected, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, rejected.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(adminCtx, adminWho, rejected.ID, ActionReject, "发票缺失")
	require.NoError(t, err)

	rejectEvents := outboxForExpense(t, database, tenant, rejected.ID)
	require.Len(t, rejectEvents, 2)
	assert.Equal(t, string(events.TypeExpenseRejected), rejectEvents[1].Type)
	snap := decodeSnapshot(t, rejectEvents[1].Payload)
	assert.Equal(t, "rejected", snap.Status)
	require.NotNil(t, snap.DecidedBy)
	assert.Equal(t, admin, *snap.DecidedBy)

	raw := string(rejectEvents[1].Payload)
	assert.NotContains(t, raw, "Acme Air", "商户名不应进入事件总线")
	assert.NotContains(t, raw, "机票", "描述不应进入事件总线")
	assert.NotContains(t, raw, "发票缺失", "驳回理由不应进入事件总线")
	assert.NotContains(t, raw, "merchant")
	assert.NotContains(t, raw, "description")
	assert.NotContains(t, raw, "rejection_reason")

	cancelled, err := svc.CreateDraft(memberCtx, memberWho, validInput())
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, cancelled.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, memberWho, cancelled.ID, ActionCancel, "")
	require.NoError(t, err)

	cancelEvents := outboxForExpense(t, database, tenant, cancelled.ID)
	require.Len(t, cancelEvents, 2)
	assert.Equal(t, string(events.TypeExpenseCancelled), cancelEvents[1].Type)
	assert.Equal(t, "cancelled", decodeSnapshot(t, cancelEvents[1].Payload).Status)
	assert.Nil(t, decodeSnapshot(t, cancelEvents[1].Payload).DecidedBy)
}

func TestFailedActionsWriteNoOutbox(t *testing.T) {
	t.Run("预算不足的提交", func(t *testing.T) {
		database := newDB(t)
		svc := newService(t, database)
		tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
		adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
		memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
		adminWho, memberWho := actorFor(admin, tenantctx.RoleAdmin), actorFor(member, tenantctx.RoleMember)

		budget := createBudgetFor(t, svc, adminCtx, adminWho, "travel", 100_000)
		in := validInput()
		in.AmountMinor = 200_000
		e, err := svc.CreateDraft(memberCtx, memberWho, in)
		require.NoError(t, err)

		_, err = svc.ApplyAction(memberCtx, memberWho, e.ID, ActionSubmit, "")
		requireAppErr(t, err, CodeBudgetUnavailable, 409)
		assert.Empty(t, outboxForExpense(t, database, tenant, e.ID))
		assert.EqualValues(t, 0, budgetAfter(t, svc, adminCtx, adminWho, budget.ID).ReservedMinor)
	})

	t.Run("自审与非法迁移", func(t *testing.T) {
		database := newDB(t)
		svc := newService(t, database)
		tenant, owner := uuid.New(), uuid.New()
		ownerCtx := tenantCtx(t, tenant, owner, tenantctx.RoleOwner)
		ownerWho := actorFor(owner, tenantctx.RoleOwner)

		createBudgetFor(t, svc, ownerCtx, ownerWho, "travel", 100_000)
		draft, err := svc.CreateDraft(ownerCtx, ownerWho, validInput())
		require.NoError(t, err)
		_, err = svc.ApplyAction(ownerCtx, ownerWho, draft.ID, ActionSubmit, "")
		require.NoError(t, err)
		require.Len(t, outboxForExpense(t, database, tenant, draft.ID), 1)

		_, err = svc.ApplyAction(ownerCtx, ownerWho, draft.ID, ActionApprove, "")
		requireAppErr(t, err, CodeSelfApproval, 403)
		_, err = svc.ApplyAction(ownerCtx, ownerWho, draft.ID, ActionSubmit, "")
		requireAppErr(t, err, CodeStateConflict, 409)
		_, err = svc.ApplyAction(ownerCtx, ownerWho, draft.ID, ActionReject, "   ")
		require.Equal(t, 400, apperr.Status(err))

		assert.Len(t, outboxForExpense(t, database, tenant, draft.ID), 1,
			"失败的动作不能留下事件")
	})
}
