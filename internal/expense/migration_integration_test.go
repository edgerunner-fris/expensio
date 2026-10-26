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

// TestMigrationDownAndUp 验证 000002 的 down SQL 真的能回滚，
// 而不是只写了一个永远没人执行的 down 文件。
func TestMigrationDownAndUp(t *testing.T) {
	database := newDB(t)
	pg := testenv.StartPostgres(t, "expense")

	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleOwner)
	insertBudget(t, database, ctx, "travel", day("2026-10-01"), day("2026-10-31"))
	require.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.budgets`))

	pg.MigrateDownUp(t, "expense", 1)

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
		return nil
	}))
}
