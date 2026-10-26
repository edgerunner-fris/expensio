package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
)

const (
	CodeInvoiceAlreadyPaid = "billing.invoice_already_paid"
	CodePaymentMismatch    = "billing.payment_mismatch"
)

const (
	// webhookTolerance 是允许的时钟偏差：超出即视为重放。
	webhookTolerance = 5 * time.Minute
	// maxWebhookBody 限制原始请求体大小，验签与 JSON 解析都在这之内完成。
	maxWebhookBody    = 64 << 10
	maxProviderRefLen = 200
)

var (
	providerPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)
	// errDuplicateWebhook 表示同一渠道事件已经处理过：事务回滚，调用方返回成功。
	errDuplicateWebhook = errors.New("billing: 重复的支付回调")
)

// PaymentWebhookInput 是网关转交的原始回调：验签必须基于原始字节，
// 不能先反序列化再序列化，否则签名字节可能被改变。
type PaymentWebhookInput struct {
	Provider  string
	Timestamp string
	Signature string
	Body      []byte
}

type PaymentWebhookResult struct {
	EventType WebhookEventType
	InvoiceID uuid.UUID
	// Duplicate 表示这次回调此前已经处理过，本次没有再次改变任何状态。
	Duplicate bool
}

// EnableWebhooks 注入回调密钥与渠道名。密钥只来自环境变量 / K8s Secret，
// 服务不提供从数据库或请求读取密钥的路径。
func (s *Service) EnableWebhooks(secret []byte, provider string) error {
	if len(secret) < 32 {
		return errors.New("billing: 回调密钥至少 32 字节")
	}
	if !providerPattern.MatchString(provider) {
		return errors.New("billing: 回调渠道名必须是小写字母开头的 2-32 位小写字母、数字、下划线或连字符")
	}
	s.webhookSecret = append([]byte(nil), secret...)
	s.webhookProvider = provider
	return nil
}

type paymentWebhookBody struct {
	EventID     string    `json:"event_id"`
	EventType   string    `json:"event_type"`
	InvoiceID   uuid.UUID `json:"invoice_id"`
	PaymentID   string    `json:"payment_id,omitempty"`
	AmountMinor int64     `json:"amount_minor,omitempty"`
	Currency    string    `json:"currency,omitempty"`
}

func decodePaymentWebhook(raw []byte) (paymentWebhookBody, error) {
	var body paymentWebhookBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, apperr.InvalidArgument("回调请求体格式非法")
	}

	body.EventID = strings.TrimSpace(body.EventID)
	if body.EventID == "" || len(body.EventID) > maxProviderRefLen {
		return body, apperr.InvalidArgument("event_id 不能为空且不超过 200 字节")
	}
	eventType := WebhookEventType(body.EventType)
	if !eventType.Valid() {
		return body, apperr.InvalidArgument("event_type 必须是 payment.succeeded 或 payment.failed")
	}
	if body.InvoiceID == uuid.Nil {
		return body, apperr.InvalidArgument("invoice_id 不能为空")
	}

	body.PaymentID = strings.TrimSpace(body.PaymentID)
	if eventType == WebhookPaymentSucceeded {
		if body.PaymentID == "" || len(body.PaymentID) > maxProviderRefLen {
			return body, apperr.InvalidArgument("payment_id 不能为空且不超过 200 字节")
		}
		if body.AmountMinor < money.MinMinor || body.AmountMinor > money.MaxMinor {
			return body, apperr.InvalidArgument("amount_minor 超出允许范围")
		}
		if _, err := money.ParseCurrency(body.Currency); err != nil {
			return body, err
		}
	}
	return body, nil
}

// verifyWebhook 先验签再检查时间窗（ADR-0005 的顺序）。
// 常量时间比较避免通过响应时间猜签名。
func (s *Service) verifyWebhook(in PaymentWebhookInput) error {
	mac := hmac.New(sha256.New, s.webhookSecret)
	mac.Write([]byte(in.Timestamp))
	mac.Write([]byte("."))
	mac.Write(in.Body)
	got, err := hex.DecodeString(strings.TrimSpace(in.Signature))
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return apperr.Unauthorized("回调签名校验失败")
	}

	ts, err := strconv.ParseInt(strings.TrimSpace(in.Timestamp), 10, 64)
	if err != nil {
		return apperr.Unauthorized("回调时间戳非法")
	}
	skew := s.now().Sub(time.Unix(ts, 0))
	if skew < -webhookTolerance || skew > webhookTolerance {
		return apperr.Unauthorized("回调时间戳超出允许范围")
	}
	return nil
}

// HandlePaymentWebhook 处理一次支付回调。
//
// 租户不从请求体读取：先用 invoice_id 调 app.invoice_tenant 反查数据库里的
// 可信映射，再以该租户建立 RLS 上下文。请求体只有账单、支付与金额。
func (s *Service) HandlePaymentWebhook(ctx context.Context, in PaymentWebhookInput) (PaymentWebhookResult, error) {
	res := PaymentWebhookResult{}
	if len(s.webhookSecret) == 0 {
		return res, apperr.Internal(errors.New("billing: 支付回调未配置"))
	}
	if in.Provider != s.webhookProvider {
		return res, apperr.Unauthorized("支付渠道不匹配")
	}
	if len(in.Body) == 0 || len(in.Body) > maxWebhookBody {
		return res, apperr.InvalidArgument("回调请求体大小非法")
	}
	if err := s.verifyWebhook(in); err != nil {
		return res, err
	}
	body, err := decodePaymentWebhook(in.Body)
	if err != nil {
		return res, err
	}

	tenantID, err := s.invoiceTenant(ctx, body.InvoiceID)
	if err != nil {
		return res, err
	}
	sha := sha256.Sum256(in.Body)

	var duplicate bool
	err = s.database.WithinTenantID(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dup, err := s.applyWebhook(ctx, tx, tenantID, in.Provider, body, sha[:])
		duplicate = dup
		return err
	})
	if errors.Is(err, errDuplicateWebhook) {
		res.EventType = WebhookEventType(body.EventType)
		res.InvoiceID = body.InvoiceID
		res.Duplicate = true
		return res, nil
	}
	if err != nil {
		return res, mapWebhookError(err)
	}

	res.EventType = WebhookEventType(body.EventType)
	res.InvoiceID = body.InvoiceID
	res.Duplicate = duplicate
	return res, nil
}

func (s *Service) applyWebhook(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, provider string, body paymentWebhookBody, sha []byte) (bool, error) {
	evt := &WebhookEvent{
		ID:              uuid.New(),
		TenantID:        tenantID,
		Provider:        provider,
		ProviderEventID: body.EventID,
		EventType:       WebhookEventType(body.EventType),
		InvoiceID:       body.InvoiceID,
		PayloadSHA256:   sha,
	}
	if err := s.repo.InsertWebhookEvent(ctx, tx, evt); err != nil {
		if constraint(err) == "webhook_events_provider_key" {
			return true, errDuplicateWebhook
		}
		return false, err
	}

	inv, err := s.repo.LockInvoice(ctx, tx, body.InvoiceID)
	if err != nil {
		return false, err
	}
	now := s.now().UTC()

	switch evt.EventType {
	case WebhookPaymentFailed:
		// 迟到的失败通知在已支付账单上只留痕，不改状态；钱已经收到了。
		if inv.Status == InvoicePaid {
			return false, nil
		}
		if err := s.moveLiveSubscription(ctx, tx, SubscriptionActive, SubscriptionPastDue); err != nil {
			return false, err
		}
		return false, s.insertInvoiceEvent(ctx, tx, events.TypeInvoicePaymentFailed, inv, now)

	case WebhookPaymentSucceeded:
		return s.applyPaymentSucceeded(ctx, tx, provider, body, inv, now)

	default:
		return false, apperr.InvalidArgument("不支持的回调事件类型")
	}
}

func (s *Service) applyPaymentSucceeded(ctx context.Context, tx pgx.Tx, provider string, body paymentWebhookBody, inv *Invoice, now time.Time) (bool, error) {
	if inv.Status == InvoicePaid {
		exists, err := s.repo.PaymentExists(ctx, tx, provider, body.PaymentID)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
		return false, apperr.Conflict(CodeInvoiceAlreadyPaid, "账单已支付，又收到另一笔支付，请人工核对")
	}
	if body.AmountMinor != inv.AmountMinor || body.Currency != string(inv.Currency) {
		return false, apperr.Conflict(CodePaymentMismatch, "支付金额或币种与账单不一致")
	}
	exists, err := s.repo.PaymentExists(ctx, tx, provider, body.PaymentID)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}

	payment := &Payment{
		ID:                uuid.New(),
		TenantID:          inv.TenantID,
		InvoiceID:         inv.ID,
		Provider:          provider,
		ProviderPaymentID: body.PaymentID,
		AmountMinor:       body.AmountMinor,
		Currency:          money.Currency(body.Currency),
		PaidAt:            now,
	}
	if err := s.repo.InsertPayment(ctx, tx, payment); err != nil {
		return false, err
	}
	if err := s.repo.MarkInvoicePaid(ctx, tx, inv, now); err != nil {
		if isNoRows(err) {
			return false, apperr.Conflict(CodeInvoiceAlreadyPaid, "账单已支付")
		}
		return false, err
	}
	if err := s.moveLiveSubscription(ctx, tx, SubscriptionPastDue, SubscriptionActive); err != nil {
		return false, err
	}
	return false, s.insertInvoiceEvent(ctx, tx, events.TypeInvoicePaid, inv, now)
}

func (s *Service) insertInvoiceEvent(ctx context.Context, tx pgx.Tx, t events.Type, inv *Invoice, occurredAt time.Time) error {
	env, err := newInvoiceEvent(t, inv, occurredAt)
	if err != nil {
		return err
	}
	return s.outbox.Insert(ctx, tx, events.TopicBilling, env)
}

// moveLiveSubscription 在支付结果改变订阅状态时使用；没有生效订阅或状态
// 已经是目标值时什么都不做。行锁避免并发回调把状态覆盖回去。
func (s *Service) moveLiveSubscription(ctx context.Context, tx pgx.Tx, from, to SubscriptionStatus) error {
	sub, err := s.repo.LockLiveSubscription(ctx, tx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if sub.Status != from {
		return nil
	}
	return s.repo.SetSubscriptionStatus(ctx, tx, sub, to)
}

func (s *Service) invoiceTenant(ctx context.Context, invoiceID uuid.UUID) (uuid.UUID, error) {
	var found pgtype.UUID
	err := s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT app.invoice_tenant($1)`, invoiceID).Scan(&found)
	})
	if err != nil {
		return uuid.Nil, apperr.Internal(err)
	}
	if !found.Valid {
		return uuid.Nil, apperr.NotFound("账单")
	}
	return uuid.UUID(found.Bytes), nil
}

func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

func mapWebhookError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("账单")
	}
	return apperr.Internal(err)
}
