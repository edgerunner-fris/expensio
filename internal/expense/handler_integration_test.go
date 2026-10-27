package expense

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

type envelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Details map[string]any  `json:"details"`
}

func newHTTPTestServer(t *testing.T, database *db.DB) (*gin.Engine, *auth.TokenIssuer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, signKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuer, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: "expensio-test", PrivateKey: signKey,
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
	})
	require.NoError(t, err)

	engine := gin.New()
	RegisterRoutes(engine, NewHandler(newService(t, database)), issuer)
	return engine, issuer
}

func tokenFor(t *testing.T, issuer *auth.TokenIssuer, tenantID, userID uuid.UUID, role tenantctx.Role) string {
	t.Helper()
	token, _, err := issuer.IssueAccess(userID, tenantID, role, uuid.New())
	require.NoError(t, err)
	return token
}

func doRequest(t *testing.T, engine *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "响应必须是统一 envelope")
	var out T
	require.NoError(t, json.Unmarshal(env.Data, &out))
	return out
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

func TestHTTPExpenseLifecycle(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant, user := uuid.New(), uuid.New()
	token := tokenFor(t, issuer, tenant, user, tenantctx.RoleMember)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/expenses", token, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[expenseView](t, rec)
	assert.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, user, created.SubmitterID)
	assert.Equal(t, "draft", created.Status)
	assert.Equal(t, "USD", created.Currency)
	assert.EqualValues(t, 12345, created.AmountMinor)
	assert.Equal(t, 1, created.Version)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page := decode[struct {
		Items    []expenseView `json:"items"`
		Total    int64         `json:"total"`
		Page     int           `json:"page"`
		PageSize int           `json:"page_size"`
	}](t, rec)
	assert.EqualValues(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, created.ID, page.Items[0].ID)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, created.ID, decode[expenseView](t, rec).ID)

	in := validInput()
	in.Merchant = "Updated"
	rec = doRequest(t, engine, http.MethodPut, "/api/v1/expenses/"+created.ID.String(), token,
		map[string]any{
			"version": 1, "category": in.Category, "currency": in.Currency,
			"amount_minor": in.AmountMinor, "merchant": in.Merchant,
			"description": in.Description, "occurred_at": in.OccurredAt,
		})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	updated := decode[expenseView](t, rec)
	assert.Equal(t, 2, updated.Version)
	assert.Equal(t, "Updated", updated.Merchant)

	rec = doRequest(t, engine, http.MethodPut, "/api/v1/expenses/"+created.ID.String(), token,
		map[string]any{
			"version": 1, "category": in.Category, "currency": in.Currency,
			"amount_minor": in.AmountMinor, "merchant": "Stale",
			"description": in.Description, "occurred_at": in.OccurredAt,
		})
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, CodeVersionConflict, decodeError(t, rec).Code)

	rec = doRequest(t, engine, http.MethodDelete,
		"/api/v1/expenses/"+created.ID.String()+"?version=2", token, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestHTTPAuthAndVisibility(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant, author, other := uuid.New(), uuid.New(), uuid.New()

	authorToken := tokenFor(t, issuer, tenant, author, tenantctx.RoleMember)
	otherToken := tokenFor(t, issuer, tenant, other, tenantctx.RoleMember)
	adminToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleAdmin)
	viewerToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleViewer)
	foreignToken := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleOwner)

	// 没有令牌：401。
	rec := doRequest(t, engine, http.MethodGet, "/api/v1/expenses", "", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 伪造令牌：401。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses", "not-a-jwt", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// viewer 不能写。
	rec = doRequest(t, engine, http.MethodPost, "/api/v1/expenses", viewerToken, validInput())
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequest(t, engine, http.MethodPost, "/api/v1/expenses", authorToken, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[expenseView](t, rec)

	// 同事看不到：404。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses/"+created.ID.String(), otherToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 管理员能看到。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses/"+created.ID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// 别的租户看不到。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses/"+created.ID.String(), foreignToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// scope=all 对普通成员 403；管理员 200。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses?scope=all", otherToken, nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/expenses?scope=all", adminToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHTTPBadRequests(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant, user := uuid.New(), uuid.New()
	token := tokenFor(t, issuer, tenant, user, tenantctx.RoleMember)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"非法 ID", http.MethodGet, "/api/v1/expenses/not-a-uuid", nil, http.StatusBadRequest},
		{"请求体不是 JSON", http.MethodPost, "/api/v1/expenses", "oops", http.StatusBadRequest},
		{"缺少 version", http.MethodPut, "/api/v1/expenses/" + uuid.NewString(),
			map[string]any{"category": "travel"}, http.StatusBadRequest},
		{"金额为零", http.MethodPost, "/api/v1/expenses",
			map[string]any{
				"category": "travel", "currency": "USD", "amount_minor": 0,
				"occurred_at": testClock.Add(-time.Hour),
			}, http.StatusBadRequest},
		{"非法状态过滤", http.MethodGet, "/api/v1/expenses?status=flying", nil, http.StatusBadRequest},
		{"删除缺 version", http.MethodDelete, "/api/v1/expenses/" + uuid.NewString(), nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, engine, tc.method, tc.path, token, tc.body)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			env := decodeError(t, rec)
			assert.NotEmpty(t, env.Code)
			assert.NotEmpty(t, env.Message)
		})
	}
}
