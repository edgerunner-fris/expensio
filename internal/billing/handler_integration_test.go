package billing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

const (
	testWebhookSecret   = "billing-test-webhook-secret-32-bytes!!"
	testWebhookProvider = "mockpay"
)

// billingTestNow 固定时钟：周期断言不能依赖跑测试的那一天。
var billingTestNow = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

type envelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Details map[string]any  `json:"details"`
}

func newBillingHTTPTestServer(t *testing.T, database *db.DB) (*gin.Engine, *auth.TokenIssuer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, signKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuer, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: "expensio-test", PrivateKey: signKey,
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
	})
	require.NoError(t, err)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }
	require.NoError(t, svc.EnableWebhooks([]byte(testWebhookSecret), testWebhookProvider))
	engine := gin.New()
	RegisterRoutes(engine, NewHandler(svc), issuer)
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

func TestBillingPlansAndSubscriptionHTTP(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newBillingHTTPTestServer(t, database)
	tenant, adminID := uuid.New(), uuid.New()
	adminToken := tokenFor(t, issuer, tenant, adminID, tenantctx.RoleAdmin)
	memberToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleMember)
	viewerToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleViewer)
	foreignToken := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleAdmin)

	// 套餐目录：任何已认证成员可读，价格只来自数据库种子。
	rec := doRequest(t, engine, http.MethodGet, "/api/v1/billing/plans", viewerToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	plans := decode[[]planView](t, rec)
	require.Len(t, plans, 5)
	assert.Equal(t, "starter-monthly", plans[0].Code)
	assert.EqualValues(t, 2900, plans[0].PriceMinor)
	assert.Equal(t, "USD", plans[0].Currency)
	assert.Equal(t, "monthly", plans[0].Interval)

	// 成员与查看者不能开通订阅。
	rec = doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", memberToken,
		SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", adminToken,
		SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[subscriptionView](t, rec)
	assert.Equal(t, "growth-monthly", created.PlanCode)
	assert.Equal(t, 5, created.Seats)
	assert.Equal(t, "active", created.Status)
	assert.Equal(t, "2026-10-26", created.CurrentPeriodStart)
	assert.Equal(t, "2026-11-25", created.CurrentPeriodEnd)
	assert.Equal(t, 1, created.Version)
	assert.Nil(t, created.CanceledAt)

	// 同一租户第二条生效订阅被唯一索引拒绝，映射为 409。
	rec = doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", adminToken,
		SubscriptionInput{PlanCode: "starter-monthly", Seats: 1})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeSubscriptionExists, decodeError(t, rec).Code)

	// 所有成员可读当前订阅与历史列表。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscription", viewerToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, created.ID, decode[subscriptionView](t, rec).ID)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscriptions", memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page := decode[struct {
		Items []subscriptionView `json:"items"`
		Total int64              `json:"total"`
	}](t, rec)
	assert.EqualValues(t, 1, page.Total)
	require.Len(t, page.Items, 1)

	// 邻租户读不到：RLS 让按 ID 查询直接没有行。
	rec = doRequest(t, engine, http.MethodGet,
		"/api/v1/billing/subscriptions/"+created.ID.String(), foreignToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 取消：成员被中间件挡住；版本不符返回 409。
	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+created.ID.String()+"/cancel", memberToken,
		map[string]int{"version": 1})
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+created.ID.String()+"/cancel", adminToken,
		map[string]int{"version": 99})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeSubscriptionVersionConflict, decodeError(t, rec).Code)

	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+created.ID.String()+"/cancel", adminToken,
		map[string]int{"version": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	canceled := decode[subscriptionView](t, rec)
	assert.Equal(t, "canceled", canceled.Status)
	assert.Equal(t, 2, canceled.Version)
	require.NotNil(t, canceled.CanceledAt)
	assert.WithinDuration(t, billingTestNow, *canceled.CanceledAt, time.Second)

	// canceled 是终态：再取消返回 409，而不是刷新 canceled_at。
	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+created.ID.String()+"/cancel", adminToken,
		map[string]int{"version": 2})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeSubscriptionStateConflict, decodeError(t, rec).Code)

	// 取消后当前订阅为空，但历史列表仍能看到。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscription", adminToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscriptions", adminToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page = decode[struct {
		Items []subscriptionView `json:"items"`
		Total int64              `json:"total"`
	}](t, rec)
	assert.EqualValues(t, 1, page.Total)
	assert.Equal(t, "canceled", page.Items[0].Status)

	// 取消后可以重新订阅。
	rec = doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", adminToken,
		SubscriptionInput{PlanCode: "starter-yearly", Seats: 2})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	renewed := decode[subscriptionView](t, rec)
	assert.Equal(t, "2026-10-26", renewed.CurrentPeriodStart)
	assert.Equal(t, "2027-10-25", renewed.CurrentPeriodEnd)
}

func TestBillingInvoiceHTTP(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newBillingHTTPTestServer(t, database)
	tenant := uuid.New()
	adminToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleAdmin)
	memberToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleMember)
	foreignToken := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", adminToken,
		SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	sub := decode[subscriptionView](t, rec)

	invoicePath := "/api/v1/billing/subscriptions/" + sub.ID.String() + "/invoices"

	// 成员不能出账。
	rec = doRequest(t, engine, http.MethodPost, invoicePath, memberToken, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodPost, invoicePath, adminToken, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	inv := decode[invoiceView](t, rec)
	assert.Equal(t, sub.ID, inv.SubscriptionID)
	assert.Equal(t, "open", inv.Status)
	assert.Equal(t, "USD", inv.Currency)
	assert.EqualValues(t, 49500, inv.AmountMinor) // 9900 × 5
	assert.Equal(t, "2026-10-26", inv.PeriodStart)
	assert.Equal(t, "2026-11-25", inv.PeriodEnd)
	assert.Equal(t, "2026-11-09", inv.DueOn)
	assert.True(t, strings.HasPrefix(inv.Number, "INV-202610-"), inv.Number)
	assert.Len(t, inv.Number, len("INV-202610-")+8)
	assert.Equal(t, 1, inv.Version)

	// 同一订阅周期重复出账 409，且不会生成第二张账单。
	rec = doRequest(t, engine, http.MethodPost, invoicePath, adminToken, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeInvoiceExists, decodeError(t, rec).Code)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices", memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page := decode[struct {
		Items []invoiceView `json:"items"`
		Total int64         `json:"total"`
	}](t, rec)
	assert.EqualValues(t, 1, page.Total)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices?status=paid", memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	page = decode[struct {
		Items []invoiceView `json:"items"`
		Total int64         `json:"total"`
	}](t, rec)
	assert.EqualValues(t, 0, page.Total)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices?status=void", memberToken, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/"+inv.ID.String(), memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, inv.Number, decode[invoiceView](t, rec).Number)

	// 邻租户看不到账单。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/"+inv.ID.String(), foreignToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 订阅取消后不能再出账。
	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+sub.ID.String()+"/cancel", adminToken,
		map[string]int{"version": 1})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodPost, invoicePath, adminToken, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeSubscriptionStateConflict, decodeError(t, rec).Code)

	// 非法/不存在的 ID。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/not-a-uuid", memberToken, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/"+uuid.NewString(), memberToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+uuid.NewString()+"/invoices", adminToken, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestBillingSubscriptionBadRequests(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newBillingHTTPTestServer(t, database)
	token := tokenFor(t, issuer, uuid.New(), uuid.New(), tenantctx.RoleAdmin)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"套餐不存在", http.MethodPost, "/api/v1/billing/subscriptions",
			map[string]any{"plan_code": "enterprise-monthly", "seats": 1}},
		{"套餐为空", http.MethodPost, "/api/v1/billing/subscriptions",
			map[string]any{"plan_code": "  ", "seats": 1}},
		{"席位为零", http.MethodPost, "/api/v1/billing/subscriptions",
			map[string]any{"plan_code": "starter-monthly", "seats": 0}},
		{"席位超上限", http.MethodPost, "/api/v1/billing/subscriptions",
			map[string]any{"plan_code": "starter-monthly", "seats": 1001}},
		{"请求体不是对象", http.MethodPost, "/api/v1/billing/subscriptions", "oops"},
		{"非法订阅 ID", http.MethodGet, "/api/v1/billing/subscriptions/not-a-uuid", nil},
		{"取消缺 version", http.MethodPost,
			"/api/v1/billing/subscriptions/" + uuid.NewString() + "/cancel", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, engine, tc.method, tc.path, token, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.NotEmpty(t, decodeError(t, rec).Code)
		})
	}
}
