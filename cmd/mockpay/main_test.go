package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testInvoice = "0d5f7a3e-4b1c-4f3a-9d2e-6c1b8a7f5e4d"

func TestBuildBodySucceeded(t *testing.T) {
	body, err := buildBody(testInvoice, "payment.succeeded", "", "", 9900, "usd")
	require.NoError(t, err)

	assert.Equal(t, testInvoice, body.InvoiceID)
	assert.Equal(t, "payment.succeeded", body.EventType)
	assert.NotEmpty(t, body.EventID)
	assert.NotEmpty(t, body.PaymentID)
	assert.Equal(t, int64(9900), body.AmountMinor)
	assert.Equal(t, "USD", body.Currency)
}

func TestBuildBodyFailedOmitsPaymentFields(t *testing.T) {
	body, err := buildBody(testInvoice, "payment.failed", "pay-1", "evt-1", 0, "")
	require.NoError(t, err)

	assert.Equal(t, "payment.failed", body.EventType)
	assert.Empty(t, body.PaymentID)
	assert.Zero(t, body.AmountMinor)
	assert.Empty(t, body.Currency)
}

func TestBuildBodyRejectsInvalidInput(t *testing.T) {
	_, err := buildBody("not-a-uuid", "payment.succeeded", "", "", 100, "USD")
	require.Error(t, err)

	_, err = buildBody(testInvoice, "payment.refunded", "", "", 100, "USD")
	require.Error(t, err)

	_, err = buildBody(testInvoice, "payment.succeeded", "", "", 0, "USD")
	require.Error(t, err)

	_, err = buildBody(testInvoice, "payment.succeeded", "", "", 100, "US")
	require.Error(t, err)
}

func TestParseEndpoint(t *testing.T) {
	got, err := parseEndpoint("http://localhost:8080/api/v1/webhooks/payments")
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080/api/v1/webhooks/payments", got)

	_, err = parseEndpoint("ftp://example.com/hook")
	require.Error(t, err)

	_, err = parseEndpoint("http://user:pass@example.com/hook")
	require.Error(t, err)

	_, err = parseEndpoint("/just/a/path")
	require.Error(t, err)
}

// 签名格式必须与 billing.verifyWebhook 一致：hex(HMAC-SHA256(secret, ts + "." + body))。
func TestSignMatchesWebhookFormat(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	ts := "1793000000"
	body := []byte(`{"event_id":"evt-1"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	assert.Equal(t, want, sign(secret, ts, body))
	assert.Len(t, sign(secret, ts, body), 64)
}
