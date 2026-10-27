package report

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

func decodeDashboard(t *testing.T, rec *httptest.ResponseRecorder) Dashboard {
	t.Helper()
	var env responseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "ok", env.Code, rec.Body.String())
	var out Dashboard
	require.NoError(t, json.Unmarshal(env.Data, &out))
	return out
}

func TestDashboardAggregates(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)

	tenantID := uuid.New()
	adminID, memberA, memberB := uuid.New(), uuid.New(), uuid.New()

	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "travel", "USD", 10000, october(2))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "approved", "meals", "USD", 2500, october(2))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "submitted", "travel", "EUR", 3000, october(5))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "reimbursed", "office", "USD", 4000, october(5))

	adminToken := tokenFor(t, issuer, tenantID, adminID, tenantctx.RoleAdmin)
	rec := doGet(t, engine, "/api/v1/reports/dashboard?from=2026-10-01&to=2026-10-31", adminToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := decodeDashboard(t, rec)
	assert.Equal(t, ScopeAll, got.Scope)
	assert.Equal(t, "2026-10-01", got.From)
	assert.Equal(t, "2026-10-31", got.To)

	require.Len(t, got.CurrencyTotals, 2)
	assert.Equal(t, CurrencyTotal{Currency: "EUR", AmountMinor: 3000, ExpenseCount: 1}, got.CurrencyTotals[0])
	assert.Equal(t, CurrencyTotal{Currency: "USD", AmountMinor: 16500, ExpenseCount: 3}, got.CurrencyTotals[1])

	statuses := map[string]StatusTotal{}
	for _, s := range got.StatusTotals {
		statuses[s.Status+"/"+s.Currency] = s
	}
	assert.Equal(t, int64(2500), statuses["approved/USD"].AmountMinor)
	assert.Equal(t, int64(4000), statuses["reimbursed/USD"].AmountMinor)
	assert.Equal(t, int64(10000), statuses["submitted/USD"].AmountMinor)
	assert.Equal(t, int64(3000), statuses["submitted/EUR"].AmountMinor)

	categories := map[string]int64{}
	for _, c := range got.CategoryTotals {
		categories[c.Group+"/"+c.Currency] = c.AmountMinor
	}
	assert.Equal(t, int64(2500), categories["meals/USD"])
	assert.Equal(t, int64(4000), categories["office/USD"])
	assert.Equal(t, int64(10000), categories["travel/USD"])
	assert.Equal(t, int64(3000), categories["travel/EUR"])

	// 按 UTC 自然日归组；同一天的不同币种各自一行。
	require.Len(t, got.DailyTotals, 3)
	assert.Equal(t, DailyTotal{Date: "2026-10-02", Currency: "USD", AmountMinor: 12500, ExpenseCount: 2}, got.DailyTotals[0])
	assert.Equal(t, DailyTotal{Date: "2026-10-05", Currency: "EUR", AmountMinor: 3000, ExpenseCount: 1}, got.DailyTotals[1])
	assert.Equal(t, DailyTotal{Date: "2026-10-05", Currency: "USD", AmountMinor: 4000, ExpenseCount: 1}, got.DailyTotals[2])

	// 成员默认只看自己，且不能显式请求全租户。
	memberToken := tokenFor(t, issuer, tenantID, memberA, tenantctx.RoleMember)
	rec = doGet(t, engine, "/api/v1/reports/dashboard?from=2026-10-01&to=2026-10-31", memberToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mine := decodeDashboard(t, rec)
	assert.Equal(t, ScopeMine, mine.Scope)
	require.Len(t, mine.CurrencyTotals, 1)
	assert.Equal(t, int64(12500), mine.CurrencyTotals[0].AmountMinor)

	rec = doGet(t, engine,
		"/api/v1/reports/dashboard?from=2026-10-01&to=2026-10-31&scope=all", memberToken)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = doGet(t, engine, "/api/v1/reports/dashboard?to=2026-10-31", adminToken)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 空区间返回空数组而不是 null。
	rec = doGet(t, engine, "/api/v1/reports/dashboard?from=2026-01-01&to=2026-01-31", adminToken)
	require.Equal(t, http.StatusOK, rec.Code)
	empty := decodeDashboard(t, rec)
	assert.NotNil(t, empty.CurrencyTotals)
	assert.Empty(t, empty.DailyTotals)
}

func TestExportCSV(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)

	tenantID := uuid.New()
	adminID := uuid.New()
	approvedID := uuid.New()
	insertSummaryFact(t, database, tenantID, uuid.New(), adminID, "submitted", "travel", "USD", 10000, october(2))
	insertSummaryFact(t, database, tenantID, approvedID, adminID, "approved", "meals", "USD", 2550, october(5))
	insertSummaryFact(t, database, tenantID, uuid.New(), adminID, "submitted", "office", "EUR", 3000, october(9))

	token := tokenFor(t, issuer, tenantID, adminID, tenantctx.RoleAdmin)
	rec := doGet(t, engine, "/api/v1/reports/expenses.csv?from=2026-10-01&to=2026-10-31", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/csv")
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "expenses-2026-10-01-2026-10-31.csv")

	body := rec.Body.Bytes()
	require.True(t, strings.HasPrefix(string(body), "\xEF\xBB\xBF"), "CSV 必须带 UTF-8 BOM")

	rows, err := csv.NewReader(strings.NewReader(string(body[3:]))).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, exportHeader, rows[0])

	// 行按发生时间排序；金额同时给主单位与最小单位。
	assert.Equal(t, "submitted", rows[1][1])
	assert.Equal(t, "travel", rows[1][2])
	assert.Equal(t, "100.00", rows[1][4])
	assert.Equal(t, "10000", rows[1][5])
	assert.Equal(t, "meals", rows[2][2])
	assert.Equal(t, "25.50", rows[2][4])
	assert.Equal(t, approvedID.String(), rows[2][0])

	// 过滤条件与 summary 一致：只导出 approved。
	rec = doGet(t, engine,
		"/api/v1/reports/expenses.csv?from=2026-10-01&to=2026-10-31&status=approved", token)
	require.Equal(t, http.StatusOK, rec.Code)
	rows, err = csv.NewReader(strings.NewReader(string(rec.Body.Bytes()[3:]))).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, approvedID.String(), rows[1][0])

	// 普通成员不能借导出拿到全租户明细。
	memberToken := tokenFor(t, issuer, tenantID, uuid.New(), tenantctx.RoleMember)
	rec = doGet(t, engine,
		"/api/v1/reports/expenses.csv?from=2026-10-01&to=2026-10-31&scope=all", memberToken)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// 参数错误仍是 JSON 错误，不会写出半截 CSV。
	rec = doGet(t, engine, "/api/v1/reports/expenses.csv?from=oops&to=2026-10-31", token)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestExportRejectsTooManyRows(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)
	tenantID, userID := uuid.New(), uuid.New()

	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.expense_facts
				    (tenant_id, expense_id, submitter_id, status, category, currency,
				     amount_minor, occurred_at, version, expense_created_at,
				     expense_updated_at, last_event_id)
				SELECT $1, gen_random_uuid(), $2, 'submitted', 'travel', 'USD',
				       100, $3, 2, $3, $3, gen_random_uuid()
				FROM generate_series(1, 50001)`, tenantID, userID, october(3))
			return err
		}))

	token := tokenFor(t, issuer, tenantID, userID, tenantctx.RoleAdmin)
	rec := doGet(t, engine, "/api/v1/reports/expenses.csv?from=2026-10-01&to=2026-10-31", token)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, decodeErrorEnvelope(t, rec).Message, "50000")
}

func TestDashboardCacheInvalidationAndFallback(t *testing.T) {
	database, _ := newDB(t)
	addr := testenv.StartRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	cache := NewDashboardCache(rdb, zap.NewNop())
	svc := NewServiceWithCache(database, cache)

	tenantID, userID := uuid.New(), uuid.New()
	actor := tenantctx.Tenant{ID: tenantID, UserID: userID, Role: tenantctx.RoleAdmin}
	q := DashboardQuery{From: "2026-10-01", To: "2026-10-31"}
	ctx, err := tenantctx.With(context.Background(), actor)
	require.NoError(t, err)

	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "submitted", "travel", "USD", 10000, october(2))
	first, err := svc.Dashboard(ctx, actor, q)
	require.NoError(t, err)
	require.Len(t, first.CurrencyTotals, 1)
	assert.Equal(t, int64(10000), first.CurrencyTotals[0].AmountMinor)

	// 绕过消费者直接改库：第二次仍读缓存，证明命中。
	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "submitted", "meals", "USD", 5000, october(3))
	cached, err := svc.Dashboard(ctx, actor, q)
	require.NoError(t, err)
	assert.Equal(t, int64(10000), cached.CurrencyTotals[0].AmountMinor)

	// 消费者推进版本号后，旧键不再被读到。
	cache.Bump(ctx, tenantID)
	fresh, err := svc.Dashboard(ctx, actor, q)
	require.NoError(t, err)
	assert.Equal(t, int64(15000), fresh.CurrencyTotals[0].AmountMinor)

	// Redis 不可达时回退到现算，而不是返回错误或空数据。
	down := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1,
	})
	t.Cleanup(func() { _ = down.Close() })
	disabled := NewServiceWithCache(database, NewDashboardCache(down, zap.NewNop()))

	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "submitted", "office", "USD", 2000, october(4))
	live, err := disabled.Dashboard(ctx, actor, q)
	require.NoError(t, err)
	assert.Equal(t, int64(17000), live.CurrencyTotals[0].AmountMinor)
}

// 回归：scope=mine 的缓存键必须带上提交人。
// 同一租户的两个成员用同一天范围查询时，若共用键，后者会读到前者的仪表盘。
func TestDashboardCacheSeparatesMembers(t *testing.T) {
	database, _ := newDB(t)
	addr := testenv.StartRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	cache := NewDashboardCache(rdb, zap.NewNop())
	svc := NewServiceWithCache(database, cache)

	tenantID := uuid.New()
	memberA, memberB := uuid.New(), uuid.New()
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "travel", "USD", 10000, october(2))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "submitted", "meals", "USD", 3000, october(3))

	q := DashboardQuery{From: "2026-10-01", To: "2026-10-31"}

	a := tenantctx.Tenant{ID: tenantID, UserID: memberA, Role: tenantctx.RoleMember}
	ctxA, err := tenantctx.With(context.Background(), a)
	require.NoError(t, err)
	first, err := svc.Dashboard(ctxA, a, q)
	require.NoError(t, err)
	assert.Equal(t, ScopeMine, first.Scope)
	require.Len(t, first.CurrencyTotals, 1)
	assert.Equal(t, int64(10000), first.CurrencyTotals[0].AmountMinor)

	// 绕过消费者给 A 再插一笔：B 的请求若命中 A 的键，下面的断言就会暴露。
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "office", "USD", 5000, october(4))

	b := tenantctx.Tenant{ID: tenantID, UserID: memberB, Role: tenantctx.RoleMember}
	ctxB, err := tenantctx.With(context.Background(), b)
	require.NoError(t, err)
	second, err := svc.Dashboard(ctxB, b, q)
	require.NoError(t, err)
	require.Len(t, second.CurrencyTotals, 1)
	assert.Equal(t, int64(3000), second.CurrencyTotals[0].AmountMinor)

	// A 再次查询仍读自己的缓存，没有被 B 的写入覆盖。
	cached, err := svc.Dashboard(ctxA, a, q)
	require.NoError(t, err)
	require.Len(t, cached.CurrencyTotals, 1)
	assert.Equal(t, int64(10000), cached.CurrencyTotals[0].AmountMinor)

	// 版本号推进后，A 才看到新的事实行。
	cache.Bump(ctxA, tenantID)
	fresh, err := svc.Dashboard(ctxA, a, q)
	require.NoError(t, err)
	require.Len(t, fresh.CurrencyTotals, 1)
	assert.Equal(t, int64(15000), fresh.CurrencyTotals[0].AmountMinor)
}

func TestConsumerBumpsDashboardCacheVersion(t *testing.T) {
	database, _ := newDB(t)
	addr := testenv.StartRedis(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })

	cache := NewDashboardCache(rdb, zap.NewNop())
	consumer := &Consumer{
		database:     database,
		logger:       zap.NewNop(),
		consumerName: "cache-test",
		cache:        cache,
	}

	tenantID, expenseID, submitterID := uuid.New(), uuid.New(), uuid.New()
	env := mustEnvelope(t, tenantID, snapshot(tenantID, expenseID, submitterID, "submitted", 2))
	raw, err := env.Encode()
	require.NoError(t, err)

	require.NoError(t, consumer.Handle(context.Background(), kafka.Message{Value: raw}))
	version, err := rdb.Get(context.Background(), versionKey(tenantID)).Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(1), version)

	// 重复投递也推进版本：它可能修复了缺失的事实行，缓存更不能继续用。
	require.NoError(t, consumer.Handle(context.Background(), kafka.Message{Value: raw}))
	version, err = rdb.Get(context.Background(), versionKey(tenantID)).Int64()
	require.NoError(t, err)
	assert.Equal(t, int64(2), version)
}
