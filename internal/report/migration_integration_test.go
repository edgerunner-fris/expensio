package report

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

var testClock = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

func newDB(t *testing.T) (*db.DB, *testenv.Postgres) {
	t.Helper()
	pg := testenv.StartPostgres(t, "report")
	database, err := db.Open(context.Background(), pg.AppDSN("report"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database, pg
}

func insertFact(t *testing.T, database *db.DB, tenantID, expenseID uuid.UUID) {
	t.Helper()
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.expense_facts
				    (tenant_id, expense_id, submitter_id, status, category, currency,
				     amount_minor, occurred_at, version, expense_created_at,
				     expense_updated_at, last_event_id)
				VALUES ($1, $2, $3, 'submitted', 'travel', 'USD',
				        12345, $4, 2, $4, $4, $5)`,
				tenantID, expenseID, uuid.New(), testClock, uuid.New())
			return err
		}))
}

func countFacts(t *testing.T, database *db.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.expense_facts`).Scan(&n)
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

func TestReadModelRLSIsolatesTenants(t *testing.T) {
	database, _ := newDB(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	expenseID := uuid.New()
	insertFact(t, database, tenantA, expenseID)

	assert.Equal(t, 1, countFacts(t, database, tenantA))
	assert.Zero(t, countFacts(t, database, tenantB), "邻居租户必须看不到事实行")

	// 没有租户上下文时策略不通过——不是报错，而是零行。
	var visible int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.expense_facts`).Scan(&visible)
	}))
	assert.Zero(t, visible, "未设置 app.tenant_id 时必须零行")
}

func TestProcessedEventsIsAppendOnly(t *testing.T) {
	database, _ := newDB(t)
	tenantID, eventID := uuid.New(), uuid.New()

	insert := func() error {
		return database.WithinTenantID(context.Background(), tenantID,
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO app.processed_events (tenant_id, consumer, event_id)
					VALUES ($1, 'report-expense-v1', $2)`, tenantID, eventID)
				return err
			})
	}
	require.NoError(t, insert())
	assert.Equal(t, "23505", sqlState(insert()), "同一消费者重复事件必须撞主键")

	err := database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `DELETE FROM app.processed_events WHERE event_id = $1`, eventID)
			return execErr
		})
	require.Error(t, err)
	assert.Equal(t, "42501", sqlState(err), "运行时角色不能删除幂等记录")
}

func TestReadModelMigrationDownAndUp(t *testing.T) {
	database, pg := newDB(t)
	tenantID := uuid.New()
	insertFact(t, database, tenantID, uuid.New())
	require.Equal(t, 1, countFacts(t, database, tenantID))

	pg.MigrateDownUp(t, "report", 1)

	fresh, err := db.Open(context.Background(), pg.AppDSN("report"), 5)
	require.NoError(t, err)
	t.Cleanup(fresh.Close)

	var facts, processed int
	require.NoError(t, fresh.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.expense_facts`).Scan(&facts); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.processed_events`).Scan(&processed); err != nil {
			return err
		}
		var rls, forced bool
		if err := tx.QueryRow(ctx, `
			SELECT relrowsecurity, relforcerowsecurity
			FROM pg_class WHERE oid = 'app.expense_facts'::regclass`).Scan(&rls, &forced); err != nil {
			return err
		}
		assert.True(t, rls)
		assert.True(t, forced)
		return nil
	}))
	assert.Zero(t, facts)
	assert.Zero(t, processed)
}
