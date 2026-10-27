package expense

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// TestMigrationDownAndUp 验证 000002..000005 的 down SQL 真的能回滚，
// 而不是只写了一个永远没人执行的 down 文件。
// 回滚 4 步会删掉协作、outbox 与业务表；只回滚 1 步仅删除协作表。
func TestMigrationDownAndUp(t *testing.T) {
	database := newDB(t)
	pg := testenv.StartPostgres(t, "expense")

	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)
	insertBudget(t, database, ctx, "travel", day("2026-10-01"), day("2026-10-31"))
	require.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.budgets`))

	pg.MigrateDownUp(t, "expense", 4)

	// DDL 会重建表，换一个新连接池，避免复用可能已失效的 prepared statement。
	fresh, err := db.Open(context.Background(), pg.AppDSN("expense"), 5)
	require.NoError(t, err)
	t.Cleanup(fresh.Close)

	// 回滚会删表，重新 up 后表应回来且为空。
	assert.Zero(t, countRows(t, fresh, ctx, `SELECT count(*) FROM app.budgets`),
		"down 必须删除业务表，up 后应为全新结构")

	require.NoError(t, fresh.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var rls, forced bool
		if err := tx.QueryRow(ctx, `
			SELECT relrowsecurity, relforcerowsecurity
			FROM pg_class WHERE oid = 'app.expenses'::regclass`).Scan(&rls, &forced); err != nil {
			return err
		}
		assert.True(t, rls)
		assert.True(t, forced)

		var transitions int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.expense_status_transitions`).Scan(&transitions); err != nil {
			return err
		}
		assert.Equal(t, 5, transitions)

		// 协作表同样强制 RLS：附件键与审批记录都是租户数据。
		for _, table := range []string{"app.expense_approvals", "app.expense_comments", "app.expense_attachments"} {
			var tableRLS, tableForced bool
			if err := tx.QueryRow(ctx, `
				SELECT relrowsecurity, relforcerowsecurity
				FROM pg_class WHERE oid = $1::regclass`, table).Scan(&tableRLS, &tableForced); err != nil {
				return err
			}
			assert.True(t, tableRLS, "%s 必须启用 RLS", table)
			assert.True(t, tableForced, "%s 必须强制 RLS（表所有者也不例外）", table)
		}

		// 分级审批的进度列由迁移回填，重新 up 后默认值为单级。
		var levels int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM app.expenses
			WHERE required_approval_levels <> 1 OR approval_level <> 0`).Scan(&levels); err != nil {
			return err
		}
		assert.Zero(t, levels, "新建费用单默认要求一级审批")

		// outbox 刻意不开 RLS：投递器要跨租户扫描，且没有对外读路径。
		var outboxRLS bool
		if err := tx.QueryRow(ctx, `
			SELECT relrowsecurity FROM pg_class
			WHERE oid = 'app.outbox_events'::regclass`).Scan(&outboxRLS); err != nil {
			return err
		}
		assert.False(t, outboxRLS)

		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.outbox_events`).Scan(&pending); err != nil {
			return err
		}
		assert.Zero(t, pending, "重新迁移后 outbox 应为空")
		return nil
	}))
}
