package expense

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestBudgetHTTP(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant := uuid.New()
	adminToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleAdmin)
	memberToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleMember)
	foreignToken := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleAdmin)

	// 成员不能建预算：中间件先拦，service 里还有一层。
	rec := doRequest(t, engine, http.MethodPost, "/api/v1/budgets", memberToken, newBudgetInput())
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodPost, "/api/v1/budgets", adminToken, newBudgetInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[budgetView](t, rec)
	assert.Equal(t, "2026-10-01", created.PeriodStart)
	assert.Equal(t, "2026-10-31", created.PeriodEnd)
	assert.EqualValues(t, 100_000, created.AvailableMinor)
	assert.Equal(t, 1, created.Version)

	// 成员可读。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/budgets", memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page := decode[struct {
		Items []budgetView `json:"items"`
		Total int64        `json:"total"`
	}](t, rec)
	assert.EqualValues(t, 1, page.Total)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/budgets/"+created.ID.String(), memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// 别的租户看不到。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/budgets/"+created.ID.String(), foreignToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	in := BudgetUpdateInput{
		Version: 1, Name: "updated", PeriodStart: "2026-10-01", PeriodEnd: "2026-10-31",
		AmountMinor: 200_000,
	}
	rec = doRequest(t, engine, http.MethodPut, "/api/v1/budgets/"+created.ID.String(), adminToken, in)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	updated := decode[budgetView](t, rec)
	assert.Equal(t, 2, updated.Version)
	assert.Equal(t, "updated", updated.Name)

	in.Version = 1
	rec = doRequest(t, engine, http.MethodPut, "/api/v1/budgets/"+created.ID.String(), adminToken, in)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, CodeBudgetVersionConflict, decodeError(t, rec).Code)

	rec = doRequest(t, engine, http.MethodDelete,
		"/api/v1/budgets/"+created.ID.String()+"?version=2", memberToken, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequest(t, engine, http.MethodDelete,
		"/api/v1/budgets/"+created.ID.String()+"?version=2", adminToken, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/budgets/"+created.ID.String(), adminToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBudgetHTTPBadRequests(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	token := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleAdmin)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"非法 ID", http.MethodGet, "/api/v1/budgets/not-a-uuid", nil},
		{"日期格式", http.MethodPost, "/api/v1/budgets", map[string]any{
			"name": "bad", "category": "travel", "currency": "USD",
			"period_start": "2026/10/01", "period_end": "2026-10-31", "amount_minor": 100,
		}},
		{"期间倒置", http.MethodPost, "/api/v1/budgets", map[string]any{
			"name": "bad", "category": "travel", "currency": "USD",
			"period_start": "2026-11-01", "period_end": "2026-10-31", "amount_minor": 100,
		}},
		{"金额为零", http.MethodPost, "/api/v1/budgets", map[string]any{
			"name": "bad", "category": "travel", "currency": "USD",
			"period_start": "2026-10-01", "period_end": "2026-10-31", "amount_minor": 0,
		}},
		{"删除缺 version", http.MethodDelete, "/api/v1/budgets/" + uuid.NewString(), nil},
		{"请求体不是对象", http.MethodPost, "/api/v1/budgets", "oops"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, engine, tc.method, tc.path, token, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			env := decodeError(t, rec)
			assert.NotEmpty(t, env.Code)
		})
	}
}
