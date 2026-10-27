package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const reportTestJWTSecret = "report-test-secret-at-least-32-bytes!"

type responseEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Details map[string]any  `json:"details"`
}

func newSummaryServer(t *testing.T, database *db.DB) (*gin.Engine, *auth.TokenIssuer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	issuer, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: "expensio-test", HS256Secret: reportTestJWTSecret,
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
	})
	require.NoError(t, err)

	engine := gin.New()
	RegisterRoutes(engine, NewHandler(NewService(database)), issuer)
	return engine, issuer
}

func tokenFor(t *testing.T, issuer *auth.TokenIssuer, tenantID, userID uuid.UUID, role tenantctx.Role) string {
	t.Helper()
	token, _, err := issuer.IssueAccess(userID, tenantID, role, uuid.New())
	require.NoError(t, err)
	return token
}

func doGet(t *testing.T, engine *gin.Engine, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func decodeSummary(t *testing.T, rec *httptest.ResponseRecorder) Summary {
	t.Helper()
	var env responseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "ok", env.Code, rec.Body.String())
	var out Summary
	require.NoError(t, json.Unmarshal(env.Data, &out))
	return out
}

func decodeErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) responseEnvelope {
	t.Helper()
	var env responseEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

func insertSummaryFact(t *testing.T, database *db.DB, tenantID, expenseID, submitterID uuid.UUID,
	status, category, currency string, amount int64, occurredAt time.Time) {
	t.Helper()
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.expense_facts
				    (tenant_id, expense_id, submitter_id, status, category, currency,
				     amount_minor, occurred_at, version, expense_created_at,
				     expense_updated_at, last_event_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 2, $8, $8, $9)`,
				tenantID, expenseID, submitterID, status, category, currency,
				amount, occurredAt, uuid.New())
			return err
		}))
}

func october(day int) time.Time {
	return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC)
}

type totalKey struct {
	Group    string
	Currency string
}

func TestSummarySeparatesCurrenciesAndGroups(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)

	tenantID := uuid.New()
	adminID, memberA, memberB := uuid.New(), uuid.New(), uuid.New()

	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "travel", "USD", 10000, october(2))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "approved", "travel", "USD", 2500, october(5))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "meals", "USD", 3000, october(9))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "reimbursed", "travel", "EUR", 5000, october(11))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "approved", "meals", "EUR", 1000, october(20))

	token := tokenFor(t, issuer, tenantID, adminID, tenantctx.RoleAdmin)
	rec := doGet(t, engine, "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31", token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	summary := decodeSummary(t, rec)
	assert.Equal(t, ScopeAll, summary.Scope, "管理员不传 scope 时默认全租户")
	assert.Equal(t, GroupByCategory, summary.GroupBy)
	assert.Equal(t, "2026-10-01", summary.From)
	assert.Equal(t, "2026-10-31", summary.To)

	// 每种币种一行，绝不出现把 USD 与 EUR 加在一起的数字。
	require.Len(t, summary.CurrencyTotals, 2)
	totals := map[string]CurrencyTotal{}
	for _, ct := range summary.CurrencyTotals {
		totals[ct.Currency] = ct
	}
	assert.Equal(t, int64(15500), totals["USD"].AmountMinor)
	assert.Equal(t, int64(3), totals["USD"].ExpenseCount)
	assert.Equal(t, int64(6000), totals["EUR"].AmountMinor)
	assert.Equal(t, int64(2), totals["EUR"].ExpenseCount)
	assert.NotContains(t, totals, "", "币种不能在聚合中丢失")

	// 分组行同样自带币种。
	require.Len(t, summary.Groups, 4)
	groups := map[totalKey]GroupTotal{}
	for _, g := range summary.Groups {
		groups[totalKey{Group: g.Group, Currency: g.Currency}] = g
	}
	assert.Equal(t, int64(12500), groups[totalKey{"travel", "USD"}].AmountMinor)
	assert.Equal(t, int64(2), groups[totalKey{"travel", "USD"}].ExpenseCount)
	assert.Equal(t, int64(3000), groups[totalKey{"meals", "USD"}].AmountMinor)
	assert.Equal(t, int64(5000), groups[totalKey{"travel", "EUR"}].AmountMinor)
	assert.Equal(t, int64(1000), groups[totalKey{"meals", "EUR"}].AmountMinor)
}

func TestSummaryScopeRules(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)

	tenantID := uuid.New()
	memberA, memberB := uuid.New(), uuid.New()
	insertSummaryFact(t, database, tenantID, uuid.New(), memberA, "submitted", "travel", "USD", 1000, october(3))
	insertSummaryFact(t, database, tenantID, uuid.New(), memberB, "submitted", "travel", "USD", 2000, october(4))

	const path = "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31"

	t.Run("成员默认只看本人", func(t *testing.T) {
		token := tokenFor(t, issuer, tenantID, memberA, tenantctx.RoleMember)
		summary := decodeSummary(t, doGet(t, engine, path, token))
		assert.Equal(t, ScopeMine, summary.Scope)
		require.Len(t, summary.CurrencyTotals, 1)
		assert.Equal(t, int64(1000), summary.CurrencyTotals[0].AmountMinor)
	})

	t.Run("查看者默认只看本人", func(t *testing.T) {
		token := tokenFor(t, issuer, tenantID, memberB, tenantctx.RoleViewer)
		summary := decodeSummary(t, doGet(t, engine, path, token))
		assert.Equal(t, ScopeMine, summary.Scope)
		assert.Equal(t, int64(2000), summary.CurrencyTotals[0].AmountMinor)
	})

	t.Run("成员请求 all 被拒绝", func(t *testing.T) {
		token := tokenFor(t, issuer, tenantID, memberA, tenantctx.RoleMember)
		rec := doGet(t, engine, path+"&scope=all", token)
		require.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "auth.forbidden", decodeErrorEnvelope(t, rec).Code)
	})

	t.Run("查看者请求 all 被拒绝", func(t *testing.T) {
		token := tokenFor(t, issuer, tenantID, memberB, tenantctx.RoleViewer)
		rec := doGet(t, engine, path+"&scope=all", token)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("管理员默认全租户但可收窄", func(t *testing.T) {
		token := tokenFor(t, issuer, tenantID, memberA, tenantctx.RoleAdmin)
		all := decodeSummary(t, doGet(t, engine, path, token))
		assert.Equal(t, ScopeAll, all.Scope)
		require.Len(t, all.CurrencyTotals, 1)
		assert.Equal(t, int64(3000), all.CurrencyTotals[0].AmountMinor)

		mine := decodeSummary(t, doGet(t, engine, path+"&scope=mine", token))
		assert.Equal(t, ScopeMine, mine.Scope)
		assert.Equal(t, int64(1000), mine.CurrencyTotals[0].AmountMinor)
	})

	t.Run("其它租户看不到数据", func(t *testing.T) {
		token := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleOwner)
		summary := decodeSummary(t, doGet(t, engine, path, token))
		assert.NotNil(t, summary.CurrencyTotals)
		assert.Empty(t, summary.CurrencyTotals)
		assert.NotNil(t, summary.Groups)
		assert.Empty(t, summary.Groups)
	})

	t.Run("缺少令牌", func(t *testing.T) {
		rec := doGet(t, engine, path, "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "auth.unauthorized", decodeErrorEnvelope(t, rec).Code)
	})
}

func TestSummaryGroupByStatusFiltersAndDateBoundaries(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)

	tenantID, userID := uuid.New(), uuid.New()
	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "submitted", "travel", "USD", 1000, october(5))
	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "approved", "travel", "USD", 2000, october(10))
	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "approved", "meals", "EUR", 500,
		time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC))
	insertSummaryFact(t, database, tenantID, uuid.New(), userID, "approved", "meals", "USD", 700,
		time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))

	token := tokenFor(t, issuer, tenantID, userID, tenantctx.RoleMember)
	base := "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&scope=mine"

	summary := decodeSummary(t, doGet(t, engine, base+"&group_by=status", token))
	assert.Equal(t, GroupByStatus, summary.GroupBy)
	require.Len(t, summary.Groups, 2)
	byGroup := map[string]GroupTotal{}
	for _, g := range summary.Groups {
		byGroup[g.Group] = g
	}
	assert.Equal(t, int64(1000), byGroup["submitted"].AmountMinor)
	assert.Equal(t, int64(2000), byGroup["approved"].AmountMinor)
	// 9 月 30 日与 11 月 1 日都不在范围内。
	require.Len(t, summary.CurrencyTotals, 1)
	assert.Equal(t, int64(3000), summary.CurrencyTotals[0].AmountMinor)

	filtered := decodeSummary(t, doGet(t, engine, base+"&status=approved", token))
	require.Len(t, filtered.CurrencyTotals, 1)
	assert.Equal(t, int64(2000), filtered.CurrencyTotals[0].AmountMinor)

	empty := decodeSummary(t, doGet(t, engine, base+"&category=meals", token))
	assert.NotNil(t, empty.CurrencyTotals)
	assert.Empty(t, empty.CurrencyTotals)

	// to 是闭区间：单日查询必须包含当天。
	oneDay := decodeSummary(t,
		doGet(t, engine, "/api/v1/reports/expenses/summary?from=2026-10-10&to=2026-10-10&scope=mine", token))
	require.Len(t, oneDay.CurrencyTotals, 1)
	assert.Equal(t, int64(2000), oneDay.CurrencyTotals[0].AmountMinor)
}

func TestSummaryRejectsInvalidQuery(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newSummaryServer(t, database)
	token := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleMember)

	tests := []struct {
		name string
		path string
	}{
		{"缺少 from", "/api/v1/reports/expenses/summary?to=2026-10-31"},
		{"日期格式错误", "/api/v1/reports/expenses/summary?from=2026-10-31x&to=2026-10-31"},
		{"from 晚于 to", "/api/v1/reports/expenses/summary?from=2026-11-01&to=2026-10-31"},
		{"范围超过一年", "/api/v1/reports/expenses/summary?from=2025-01-01&to=2026-01-02"},
		{"未知分组", "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&group_by=submitter"},
		{"非法状态", "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&status=draft"},
		{"非法类别", "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&category=gadgets"},
		{"非法 scope", "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&scope=everything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doGet(t, engine, tt.path, token)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, "common.invalid_argument", decodeErrorEnvelope(t, rec).Code)
		})
	}
}
