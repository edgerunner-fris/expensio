package expense

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// 测试数据的时间基准固定，避免依赖系统时钟导致跨日/跨月不稳定。
var testClock = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

// newDB 返回连接 expense_app 的句柄。必须用 app 角色，否则 RLS 不生效。
func newDB(t *testing.T) *db.DB {
	t.Helper()
	pg := testenv.StartPostgres(t, "expense")
	database, err := db.Open(context.Background(), pg.AppDSN("expense"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database
}

func tenantCtx(t *testing.T, tenantID, userID uuid.UUID, role tenantctx.Role) context.Context {
	t.Helper()
	ctx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID:     tenantID,
		Slug:   "t-" + tenantID.String()[:8],
		UserID: userID,
		Role:   role,
	})
	require.NoError(t, err)
	return ctx
}

// insertBudget 在当前租户上下文里插入一笔预算。
func insertBudget(t *testing.T, database *db.DB, ctx context.Context, category string, start, end time.Time) uuid.UUID {
	t.Helper()
	actor := mustTenant(t, ctx).UserID
	id := uuid.New()
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.budgets
			    (id, tenant_id, name, category, currency,
			     period_start, period_end, amount_minor, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'USD', $5, $6, 100000, $7, $7)`,
			id, mustTenant(t, ctx).ID, "test-budget", category,
			start.Format("2006-01-02"), end.Format("2006-01-02"), actor)
		return err
	}))
	return id
}

// insertExpense 插入一张指定状态的费用单。非 draft 状态会被数据库触发器拒绝，
// 因此负面用例用它验证"不允许凭空创建已审批单据"。
func insertExpense(t *testing.T, database *db.DB, ctx context.Context, submitter uuid.UUID, status string, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.expenses
			    (id, tenant_id, submitter_id, status, category, currency,
			     amount_minor, occurred_at, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 'travel', 'USD', $5, $6, $3, $3)`,
			id, mustTenant(t, ctx).ID, submitter, status, amount, testClock)
		return err
	}))
	return id
}

// execInTenant 执行一条预期可能失败的语句，返回原始错误供 SQLSTATE 断言。
func execInTenant(t *testing.T, database *db.DB, ctx context.Context, sql string, args ...any) error {
	t.Helper()
	return database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func mustTenant(t *testing.T, ctx context.Context) tenantctx.Tenant {
	t.Helper()
	tn, ok := tenantctx.From(ctx)
	require.True(t, ok, "测试上下文必须带租户")
	return tn
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// countRows 在当前租户上下文里执行 count 查询。
func countRows(t *testing.T, database *db.DB, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}))
	return n
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
