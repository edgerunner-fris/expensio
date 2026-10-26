package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

func newDB(t *testing.T) (*db.DB, *testenv.Postgres) {
	t.Helper()
	pg := testenv.StartPostgres(t, "billing")
	database, err := db.Open(context.Background(), pg.AppDSN("billing"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database, pg
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

func mustTenant(t *testing.T, ctx context.Context) tenantctx.Tenant {
	t.Helper()
	tenant, ok := tenantctx.From(ctx)
	require.True(t, ok)
	return tenant
}

func insertSubscription(t *testing.T, database *db.DB, ctx context.Context, plan string, seats int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tenant := mustTenant(t, ctx)
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats,
			     current_period_start, current_period_end, created_by, updated_by)
			VALUES ($1, $2, $3, $4, '2026-10-01', '2026-10-31', $5, $5)`,
			id, tenant.ID, plan, seats, tenant.UserID)
		return err
	}))
	return id
}

func insertInvoice(t *testing.T, database *db.DB, ctx context.Context, subscriptionID uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tenant := mustTenant(t, ctx)
	number := "INV-202610-" + strings.ToUpper(strings.ReplaceAll(id.String()[:8], "-", ""))
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.invoices
			    (id, tenant_id, subscription_id, number,
			     period_start, period_end, currency, amount_minor, due_on)
			VALUES ($1, $2, $3, $4, '2026-10-01', '2026-10-31', 'USD', $5, '2026-10-15')`,
			id, tenant.ID, subscriptionID, number, amount)
		return err
	}))
	return id
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func requireSQLState(t *testing.T, want string, err error) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, want, sqlState(err), "err = %v", err)
}

func countRows(t *testing.T, database *db.DB, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	}))
	return n
}

func countWithoutTenant(t *testing.T, database *db.DB, query string) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query).Scan(&n)
	}))
	return n
}
