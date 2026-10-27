package expense

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// TestSchemaApplies 确认迁移能在真实的 PostgreSQL 上跑通，
// 且关键约束不是"写在文档里"：RLS 强制、区间排斥、状态白名单种子都存在。
func TestSchemaApplies(t *testing.T) {
	database := newDB(t)
	ctx := context.Background()

	require.NoError(t, database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var ext int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM pg_extension WHERE extname = 'btree_gist'`).Scan(&ext); err != nil {
			return err
		}
		assert.Equal(t, 1, ext, "区间重叠约束依赖 btree_gist")

		for _, table := range []string{"budgets", "expenses", "approval_steps"} {
			var rls, forced bool
			if err := tx.QueryRow(ctx, `
				SELECT relrowsecurity, relforcerowsecurity
				FROM pg_class WHERE oid = ('app.' || $1)::regclass`, table).Scan(&rls, &forced); err != nil {
				return err
			}
			assert.True(t, rls, "%s 必须启用 RLS", table)
			assert.True(t, forced, "%s 必须 FORCE RLS", table)
		}

		var exclusion int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pg_constraint
			WHERE conname = 'budgets_period_no_overlap'`).Scan(&exclusion); err != nil {
			return err
		}
		assert.Equal(t, 1, exclusion, "预算区间排斥约束必须存在")

		var transitions int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.expense_status_transitions`).Scan(&transitions); err != nil {
			return err
		}
		assert.Equal(t, 5, transitions, "状态白名单必须与设计一致")

		return nil
	}))
}

func TestRLSIsolation(t *testing.T) {
	database := newDB(t)
	tenantA, userA := uuid.New(), uuid.New()
	tenantB, userB := uuid.New(), uuid.New()
	ctxA := tenantCtx(t, tenantA, userA, tenantctx.RoleOwner)
	ctxB := tenantCtx(t, tenantB, userB, tenantctx.RoleOwner)

	budgetA := insertBudget(t, database, ctxA, "travel", day("2026-10-01"), day("2026-10-31"))
	budgetB := insertBudget(t, database, ctxB, "travel", day("2026-10-01"), day("2026-10-31"))
	expenseA := insertExpense(t, database, ctxA, userA, "draft", 1000)
	insertExpense(t, database, ctxB, userB, "draft", 2000)

	assert.Equal(t, 1, countRows(t, database, ctxA, `SELECT count(*) FROM app.budgets`))
	assert.Equal(t, 1, countRows(t, database, ctxA, `SELECT count(*) FROM app.expenses`))
	assert.Equal(t, 0, countRows(t, database, ctxA,
		`SELECT count(*) FROM app.budgets WHERE id = $1`, budgetB))
	assert.Equal(t, 1, countRows(t, database, ctxA,
		`SELECT count(*) FROM app.budgets WHERE id = $1`, budgetA))

	// 没有租户上下文时，默认看到零行，而不是全量。
	var all int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.expenses`).Scan(&all)
	}))
	assert.Zero(t, all, "未设置 app.tenant_id 时不能看到任何租户数据")

	// 跨租户写入被 WITH CHECK 拒绝。
	err := execInTenant(t, database, ctxA, `
		INSERT INTO app.budgets
		    (id, tenant_id, name, category, currency, period_start, period_end,
		     amount_minor, created_by, updated_by)
		VALUES ($1, $2, 'cross', 'meals', 'USD', '2026-10-01', '2026-10-31', 1000, $3, $3)`,
		uuid.New(), tenantB, userA)
	assert.Equal(t, "42501", sqlState(err), "跨租户 INSERT 必须被 RLS 拒绝")

	// 跨租户按 ID 更新：行不可见，影响 0 行。
	var affected int64
	require.NoError(t, database.WithinTenant(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE app.expenses SET merchant = 'hacked' WHERE id = $1`, expenseA)
		affected = tag.RowsAffected()
		return err
	}))
	assert.EqualValues(t, 1, affected, "本租户的行可以更新")

	require.NoError(t, database.WithinTenant(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE app.expenses SET merchant = 'hacked'
			WHERE id IN (SELECT id FROM app.expenses WHERE tenant_id = $1)`, tenantB)
		affected = tag.RowsAffected()
		return err
	}))
	assert.Zero(t, affected, "看不到的行无法被修改")
}

func TestStatusTransitionGuard(t *testing.T) {
	database := newDB(t)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)

	// 不允许凭空创建非草稿单据。
	err := execInTenant(t, database, ctx, `
		INSERT INTO app.expenses
		    (id, tenant_id, submitter_id, status, category, currency,
		     amount_minor, occurred_at, created_by, updated_by)
		VALUES ($1, $2, $3, 'approved', 'travel', 'USD', 1000, now(), $3, $3)`,
		uuid.New(), tenant, user)
	assert.Equal(t, "23514", sqlState(err))

	id := insertExpense(t, database, ctx, user, "draft", 1000)

	// draft → approved 跳步。审批级别必须同时走完，否则先撞一致性约束，
	// 这里一并置为 required 级，确保断言的是状态机而不是新约束。
	err = execInTenant(t, database, ctx, `
		UPDATE app.expenses
		SET status = 'approved', approval_level = required_approval_levels
		WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err))
	require.NotNil(t, err)
	assert.Contains(t, err.Error(), "invalid expense status transition")

	// 合法：draft → submitted。
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.expenses SET status = 'submitted' WHERE id = $1`, id))

	// 提交后金额冻结。
	err = execInTenant(t, database, ctx,
		`UPDATE app.expenses SET amount_minor = amount_minor + 1 WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err))

	// 审批通过必须同时写入决定人/时间；级别已走完，只剩决定字段缺失。
	err = execInTenant(t, database, ctx, `
		UPDATE app.expenses
		SET status = 'approved', approval_level = required_approval_levels
		WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err))

	require.NoError(t, execInTenant(t, database, ctx, `
		UPDATE app.expenses
		SET status = 'approved', approval_level = required_approval_levels,
		    decided_by = $2, decided_at = now()
		WHERE id = $1`, id, user))

	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.expenses SET status = 'reimbursed' WHERE id = $1`, id))

	// 终态没有出边。
	err = execInTenant(t, database, ctx,
		`UPDATE app.expenses SET status = 'submitted' WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err))
}

func TestSoftDeleteRules(t *testing.T) {
	database := newDB(t)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)

	draft := insertExpense(t, database, ctx, user, "draft", 1000)
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.expenses SET deleted_at = now() WHERE id = $1`, draft))

	err := execInTenant(t, database, ctx,
		`UPDATE app.expenses SET deleted_at = NULL WHERE id = $1`, draft)
	assert.Equal(t, "23514", sqlState(err), "软删后不可恢复")

	submitted := insertExpense(t, database, ctx, user, "draft", 1000)
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.expenses SET status = 'submitted' WHERE id = $1`, submitted))

	err = execInTenant(t, database, ctx,
		`UPDATE app.expenses SET deleted_at = now() WHERE id = $1`, submitted)
	assert.Equal(t, "23514", sqlState(err), "提交后的费用单不能直接软删")
}

func TestApprovalStepsAreAppendOnly(t *testing.T) {
	database := newDB(t)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)

	expenseID := insertExpense(t, database, ctx, user, "draft", 1000)
	stepID := uuid.New()
	require.NoError(t, execInTenant(t, database, ctx, `
		INSERT INTO app.approval_steps
		    (id, tenant_id, expense_id, actor_id, action, from_status, to_status)
		VALUES ($1, $2, $3, $4, 'submit', 'draft', 'submitted')`,
		stepID, tenant, expenseID, user))

	err := execInTenant(t, database, ctx,
		`UPDATE app.approval_steps SET comment = 'tampered' WHERE id = $1`, stepID)
	assert.Equal(t, "42501", sqlState(err), "审批记录不能被修改")

	err = execInTenant(t, database, ctx,
		`DELETE FROM app.approval_steps WHERE id = $1`, stepID)
	assert.Equal(t, "42501", sqlState(err), "审批记录不能被删除")

	err = execInTenant(t, database, ctx,
		`INSERT INTO app.expense_status_transitions (from_status, to_status)
		 VALUES ('draft', 'approved')`)
	assert.Equal(t, "42501", sqlState(err), "状态白名单只能由迁移维护")

	// 动作与状态边必须一致；驳回必须带理由。
	err = execInTenant(t, database, ctx, `
		INSERT INTO app.approval_steps
		    (id, tenant_id, expense_id, actor_id, action, from_status, to_status)
		VALUES ($1, $2, $3, $4, 'approve', 'draft', 'submitted')`,
		uuid.New(), tenant, expenseID, user)
	assert.Equal(t, "23514", sqlState(err), "动作与状态边不匹配必须被拒绝")

	err = execInTenant(t, database, ctx, `
		INSERT INTO app.approval_steps
		    (id, tenant_id, expense_id, actor_id, action, from_status, to_status)
		VALUES ($1, $2, $3, $4, 'reject', 'submitted', 'rejected')`,
		uuid.New(), tenant, expenseID, user)
	assert.Equal(t, "23514", sqlState(err), "驳回必须填写理由")
}

func TestBudgetPeriodOverlap(t *testing.T) {
	database := newDB(t)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)

	first := insertBudget(t, database, ctx, "travel", day("2026-10-01"), day("2026-10-31"))

	insert := func(category, start, end string) error {
		return execInTenant(t, database, ctx, `
			INSERT INTO app.budgets
			    (id, tenant_id, name, category, currency, period_start, period_end,
			     amount_minor, created_by, updated_by)
			VALUES ($1, $2, 'overlap', $3, 'USD', $4, $5, 100000, $6, $6)`,
			uuid.New(), tenant, category, start, end, user)
	}

	err := insert("travel", "2026-10-15", "2026-11-15")
	assert.Equal(t, "23P01", sqlState(err), "同类别同币种期间重叠必须被拒绝")

	require.NoError(t, insert("travel", "2026-11-01", "2026-11-30"), "相邻期间允许")
	require.NoError(t, insert("meals", "2026-10-01", "2026-10-31"), "不同类别允许")

	// 软删之后，同一期间可以重新建预算。
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET deleted_at = now() WHERE id = $1`, first))
	require.NoError(t, insert("travel", "2026-10-01", "2026-10-31"))
}

func TestBudgetGuards(t *testing.T) {
	database := newDB(t)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)

	id := insertBudget(t, database, ctx, "travel", day("2026-10-01"), day("2026-10-31"))

	err := execInTenant(t, database, ctx,
		`UPDATE app.budgets SET reserved_minor = amount_minor + 1 WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err), "预占不能超过预算")

	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET reserved_minor = 100 WHERE id = $1`, id))
	err = execInTenant(t, database, ctx,
		`UPDATE app.budgets SET deleted_at = now() WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err), "仍有占用时不能删除预算")

	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET reserved_minor = 0 WHERE id = $1`, id))
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET deleted_at = now() WHERE id = $1`, id))

	err = execInTenant(t, database, ctx,
		`UPDATE app.budgets SET deleted_at = NULL WHERE id = $1`, id)
	assert.Equal(t, "23514", sqlState(err), "预算软删后不可恢复")
}

// TestBudgetFKIsTenantScoped 确认费用单不能引用另一个租户的预算。
func TestBudgetFKIsTenantScoped(t *testing.T) {
	database := newDB(t)
	tenantA, userA := uuid.New(), uuid.New()
	tenantB, userB := uuid.New(), uuid.New()
	ctxA := tenantCtx(t, tenantA, userA, tenantctx.RoleOwner)
	ctxB := tenantCtx(t, tenantB, userB, tenantctx.RoleOwner)

	foreignBudget := insertBudget(t, database, ctxB, "travel", day("2026-10-01"), day("2026-10-31"))

	err := execInTenant(t, database, ctxA, `
		INSERT INTO app.expenses
		    (id, tenant_id, submitter_id, status, category, currency,
		     amount_minor, occurred_at, budget_id, created_by, updated_by)
		VALUES ($1, $2, $3, 'draft', 'travel', 'USD', 1000, now(), $4, $3, $3)`,
		uuid.New(), tenantA, userA, foreignBudget)
	// tenant_id 本身合法，RLS 的 WITH CHECK 会放行；真正拦下它的是复合外键：
	// (tenantA, budgetB) 在 budgets 里不存在，且外键检查不经过 RLS。
	assert.Equal(t, "23503", sqlState(err), "跨租户预算引用必须被复合外键拒绝")

	assert.Equal(t, 0, countRows(t, database, ctxA, `SELECT count(*) FROM app.expenses`))
}
