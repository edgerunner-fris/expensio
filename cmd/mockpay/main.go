// mockpay 是本地与集成环境用的"支付渠道"：按 billing 的验签规则签名并投递
// 一条支付回调。它只做一件事——证明回调链路（签名、时间窗、幂等、对账）
// 可以在没有真实支付服务的情况下被完整驱动。
//
// 口令只从环境变量读取，不接受命令行参数：命令行会进入 shell 历史与
// ps 列表。与 billing 服务共用同一组变量，避免"测试端和真实端各用一份":
//
//	EXPENSIO_BILLING_WEBHOOK_SECRET    必填，至少 32 字节
//	EXPENSIO_BILLING_WEBHOOK_PROVIDER  可选，默认 mockpay
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	minSecretLen = 32
	maxBodyRead  = 4 << 10
)

// webhookBody 必须与 billing.paymentWebhookBody 的字段名完全一致：
// billing 用 DisallowUnknownFields 解码，多一个字段都会被拒绝。
type webhookBody struct {
	EventID     string `json:"event_id"`
	EventType   string `json:"event_type"`
	InvoiceID   string `json:"invoice_id"`
	PaymentID   string `json:"payment_id,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		target    = flag.String("url", "http://localhost:8080/api/v1/webhooks/payments", "回调接收地址")
		invoiceID = flag.String("invoice", "", "账单 ID（必填）")
		eventType = flag.String("event", "payment.succeeded", "payment.succeeded 或 payment.failed")
		paymentID = flag.String("payment-id", "", "渠道支付号；默认为随机值")
		eventID   = flag.String("event-id", "", "渠道事件号；默认随机。传入相同值可验证幂等")
		amount    = flag.Int64("amount", 0, "金额（最小货币单位，payment.succeeded 必填）")
		currency  = flag.String("currency", "USD", "三位大写币种")
		timeout   = flag.Duration("timeout", 10*time.Second, "回调请求超时")
	)
	flag.Parse()

	secret := os.Getenv("EXPENSIO_BILLING_WEBHOOK_SECRET")
	if len(secret) < minSecretLen {
		return fail("EXPENSIO_BILLING_WEBHOOK_SECRET 必须设置且至少 %d 字节", minSecretLen)
	}
	provider := strings.TrimSpace(os.Getenv("EXPENSIO_BILLING_WEBHOOK_PROVIDER"))
	if provider == "" {
		provider = "mockpay"
	}

	body, err := buildBody(*invoiceID, *eventType, *paymentID, *eventID, *amount, *currency)
	if err != nil {
		return fail("%v", err)
	}
	endpoint, err := parseEndpoint(*target)
	if err != nil {
		return fail("%v", err)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fail("序列化回调失败: %v", err)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fail("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Billing-Provider", provider)
	req.Header.Set("X-Billing-Timestamp", ts)
	req.Header.Set("X-Billing-Signature", sign(secret, ts, raw))

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fail("投递回调失败: %v", err)
	}
	defer resp.Body.Close()

	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	fmt.Printf("HTTP %d %s\n", resp.StatusCode, strings.TrimSpace(string(reply)))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 1
	}
	return 0
}

func buildBody(invoice, event, payment, eventID string, amount int64, currency string) (webhookBody, error) {
	var out webhookBody
	if _, err := uuid.Parse(strings.TrimSpace(invoice)); err != nil {
		return out, errors.New("--invoice 必须是合法的账单 UUID")
	}
	event = strings.TrimSpace(event)
	if event != "payment.succeeded" && event != "payment.failed" {
		return out, errors.New("--event 必须是 payment.succeeded 或 payment.failed")
	}
	if strings.TrimSpace(eventID) == "" {
		eventID = "evt-" + uuid.NewString()
	}
	out = webhookBody{
		EventID:   eventID,
		EventType: event,
		InvoiceID: strings.TrimSpace(invoice),
	}
	if event == "payment.failed" {
		// 失败回调不携带支付信息，与 billing 的解析规则一致。
		return out, nil
	}
	if amount <= 0 {
		return out, errors.New("payment.succeeded 必须提供 --amount")
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return out, errors.New("--currency 必须是三位大写币种")
	}
	if strings.TrimSpace(payment) == "" {
		payment = "mockpay-" + uuid.NewString()
	}
	out.PaymentID = payment
	out.AmountMinor = amount
	out.Currency = currency
	return out, nil
}

func parseEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("--url 必须是完整的 http(s) 地址，当前为 %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("--url 只支持 http/https，当前为 %q", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("--url 不能包含用户名或口令")
	}
	return u.String(), nil
}

// sign 与 billing.verifyWebhook 使用同一算法：hex(HMAC-SHA256(secret, ts + "." + body))。
func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "mockpay: "+format+"\n", args...)
	return 1
}
