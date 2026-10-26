package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func signWebhook(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func webhookBody(t *testing.T, eventType string, invoiceID uuid.UUID, eventID, paymentID string, amount int64, currency string) []byte {
	t.Helper()
	body := map[string]any{
		"event_id":   eventID,
		"event_type": eventType,
		"invoice_id": invoiceID,
	}
	if paymentID != "" {
		body["payment_id"] = paymentID
		body["amount_minor"] = amount
		body["currency"] = currency
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return raw
}

func signedInput(t *testing.T, raw []byte) PaymentWebhookInput {
	t.Helper()
	ts := strconv.FormatInt(billingTestNow.Unix(), 10)
	return PaymentWebhookInput{
		Provider:  testWebhookProvider,
		Timestamp: ts,
		Signature: signWebhook([]byte(testWebhookSecret), ts, raw),
		Body:      raw,
	}
}

func TestBillingPaymentWebhookTransitions(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }
	require.NoError(t, svc.EnableWebhooks([]byte(testWebhookSecret), testWebhookProvider))

	sub, err := svc.CreateSubscription(ctx, actor, SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.NoError(t, err)
	inv, err := svc.GenerateInvoice(ctx, actor, sub.ID)
	require.NoError(t, err)

	// 失败回调：账单保持 open，订阅进入 past_due。
	failedBody := webhookBody(t, "payment.failed", inv.ID, "evt_fail_1", "", 0, "")
	res, err := svc.HandlePaymentWebhook(context.Background(), signedInput(t, failedBody))
	require.NoError(t, err)
	assert.False(t, res.Duplicate)
	assert.Equal(t, WebhookPaymentFailed, res.EventType)

	live, err := svc.GetCurrentSubscription(ctx, actor)
	require.NoError(t, err)
	assert.Equal(t, SubscriptionPastDue, live.Status)
	assert.Equal(t, 2, live.Version)

	got, err := svc.GetInvoice(ctx, actor, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, InvoiceOpen, got.Status)
	assert.Nil(t, got.PaidAt)

	// 成功回调：账单 paid，订阅回到 active，支付流水只写一条。
	okBody := webhookBody(t, "payment.succeeded", inv.ID, "evt_ok_1", "pay_1", 49500, "USD")
	res, err = svc.HandlePaymentWebhook(context.Background(), signedInput(t, okBody))
	require.NoError(t, err)
	assert.False(t, res.Duplicate)

	got, err = svc.GetInvoice(ctx, actor, inv.ID)
	require.NoError(t, err)
	assert.Equal(t, InvoicePaid, got.Status)
	require.NotNil(t, got.PaidAt)
	assert.Equal(t, 2, got.Version)

	live, err = svc.GetCurrentSubscription(ctx, actor)
	require.NoError(t, err)
	assert.Equal(t, SubscriptionActive, live.Status)
	assert.Equal(t, 3, live.Version)

	assert.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.payments`))
	assert.Equal(t, 2, countRows(t, database, ctx, `SELECT count(*) FROM app.webhook_events`))
	rows := loadOutbox(t, database, ctx)
	assert.Equal(t, 1, countEventType(rows, "invoice.payment_failed"))
	assert.Equal(t, 1, countEventType(rows, "invoice.paid"))
	outboxAfterPaid := len(rows)

	// 同一事件重投：返回 duplicate，不重复落库。
	res, err = svc.HandlePaymentWebhook(context.Background(), signedInput(t, okBody))
	require.NoError(t, err)
	assert.True(t, res.Duplicate)
	assert.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.payments`))
	assert.Equal(t, 2, countRows(t, database, ctx, `SELECT count(*) FROM app.webhook_events`))
	assert.Len(t, loadOutbox(t, database, ctx), outboxAfterPaid)

	// 不同事件 ID、同一支付流水：同样视为已处理。
	res, err = svc.HandlePaymentWebhook(context.Background(),
		signedInput(t, webhookBody(t, "payment.succeeded", inv.ID, "evt_ok_1_retry", "pay_1", 49500, "USD")))
	require.NoError(t, err)
	assert.True(t, res.Duplicate)
	assert.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.payments`))

	// 已支付账单收到另一笔支付：409，不落流水。
	_, err = svc.HandlePaymentWebhook(context.Background(),
		signedInput(t, webhookBody(t, "payment.succeeded", inv.ID, "evt_ok_2", "pay_2", 49500, "USD")))
	require.Error(t, err)
	e, ok := apperr.As(err)
	require.True(t, ok)
	assert.Equal(t, CodeInvoiceAlreadyPaid, e.Code())
	assert.Equal(t, 1, countRows(t, database, ctx, `SELECT count(*) FROM app.payments`))
}

func doWebhookRequest(t *testing.T, engine *gin.Engine, raw []byte, provider, timestamp, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/webhooks/payments", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Provider", provider)
	req.Header.Set("X-Billing-Timestamp", timestamp)
	req.Header.Set("X-Billing-Signature", signature)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func createInvoiceFixture(t *testing.T, engine *gin.Engine, issuer *auth.TokenIssuer, tenantID uuid.UUID) (string, subscriptionView, invoiceView) {
	t.Helper()
	token := tokenFor(t, issuer, tenantID, uuid.New(), tenantctx.RoleAdmin)
	rec := doRequest(t, engine, http.MethodPost, "/api/v1/billing/subscriptions", token,
		SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	sub := decode[subscriptionView](t, rec)

	rec = doRequest(t, engine, http.MethodPost,
		"/api/v1/billing/subscriptions/"+sub.ID.String()+"/invoices", token, nil)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return token, sub, decode[invoiceView](t, rec)
}

func TestBillingPaymentWebhookHTTP(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newBillingHTTPTestServer(t, database)
	_, _, inv := createInvoiceFixture(t, engine, issuer, uuid.New())

	ts := strconv.FormatInt(billingTestNow.Unix(), 10)
	secret := []byte(testWebhookSecret)
	okBody := webhookBody(t, "payment.succeeded", inv.ID, "evt_http_1", "pay_http_1", 49500, "USD")

	// 签名错误：401，且不写任何记录。
	rec := doWebhookRequest(t, engine, okBody, testWebhookProvider, ts,
		signWebhook([]byte("wrong-secret-wrong-secret-wrong!"), ts, okBody))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// 时间戳超出 5 分钟：401，即使签名正确。
	staleTS := strconv.FormatInt(billingTestNow.Add(-6*time.Minute).Unix(), 10)
	rec = doWebhookRequest(t, engine, okBody, testWebhookProvider, staleTS,
		signWebhook(secret, staleTS, okBody))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// 渠道不匹配：401。
	rec = doWebhookRequest(t, engine, okBody, "otherpay", ts, signWebhook(secret, ts, okBody))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// 未知账单：404（租户反查不到，不可能写入任何租户的数据）。
	unknown := webhookBody(t, "payment.succeeded", uuid.New(), "evt_http_unknown", "pay_x", 100, "USD")
	rec = doWebhookRequest(t, engine, unknown, testWebhookProvider, ts, signWebhook(secret, ts, unknown))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 非法 JSON：400。
	bad := []byte("{not json")
	rec = doWebhookRequest(t, engine, bad, testWebhookProvider, ts, signWebhook(secret, ts, bad))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 正常成功：200，账单变 paid。
	rec = doWebhookRequest(t, engine, okBody, testWebhookProvider, ts, signWebhook(secret, ts, okBody))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	result := decode[struct {
		Status    string    `json:"status"`
		EventType string    `json:"event_type"`
		InvoiceID uuid.UUID `json:"invoice_id"`
		Duplicate bool      `json:"duplicate"`
	}](t, rec)
	assert.Equal(t, "processed", result.Status)
	assert.False(t, result.Duplicate)
	assert.Equal(t, inv.ID, result.InvoiceID)

	// 重投：200 duplicate。
	rec = doWebhookRequest(t, engine, okBody, testWebhookProvider, ts, signWebhook(secret, ts, okBody))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, decode[struct {
		Status    string    `json:"status"`
		EventType string    `json:"event_type"`
		InvoiceID uuid.UUID `json:"invoice_id"`
		Duplicate bool      `json:"duplicate"`
	}](t, rec).Duplicate)
}

func TestBillingPaymentWebhookMismatch(t *testing.T) {
	database, _ := newDB(t)
	engine, issuer := newBillingHTTPTestServer(t, database)
	token, sub, inv := createInvoiceFixture(t, engine, issuer, uuid.New())

	ts := strconv.FormatInt(billingTestNow.Unix(), 10)
	secret := []byte(testWebhookSecret)

	mismatch := webhookBody(t, "payment.succeeded", inv.ID, "evt_mismatch", "pay_m", 1, "USD")
	rec := doWebhookRequest(t, engine, mismatch, testWebhookProvider, ts, signWebhook(secret, ts, mismatch))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodePaymentMismatch, decodeError(t, rec).Code)

	// 金额不一致不会改变任何状态。
	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/"+inv.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "open", decode[invoiceView](t, rec).Status)

	// 失败回调把订阅推到 past_due，账单仍 open。
	failed := webhookBody(t, "payment.failed", inv.ID, "evt_failed", "", 0, "")
	rec = doWebhookRequest(t, engine, failed, testWebhookProvider, ts, signWebhook(secret, ts, failed))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscription", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "past_due", decode[subscriptionView](t, rec).Status)

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/invoices/"+inv.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "open", decode[invoiceView](t, rec).Status)

	// 迟到的成功回调恢复订阅。
	ok := webhookBody(t, "payment.succeeded", inv.ID, "evt_ok", "pay_ok", 49500, "USD")
	rec = doWebhookRequest(t, engine, ok, testWebhookProvider, ts, signWebhook(secret, ts, ok))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/billing/subscription", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	live := decode[subscriptionView](t, rec)
	assert.Equal(t, "active", live.Status)
	assert.Equal(t, sub.ID, live.ID)
}
