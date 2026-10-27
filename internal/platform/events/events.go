// Package events 定义服务之间的事件契约。
//
// 契约一旦发布就不能悄悄改变：生产者与消费者是两个独立的二进制，
// 升级顺序不受控制。因此本包只做两件事——序列化/反序列化与最基础的
// 结构校验；业务字段含义写在 ADR-0004。
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 主题与消费组是运维契约，不能由调用方随意拼接。
const (
	TopicExpense    = "expensio.expense.events.v1"
	TopicExpenseDLQ = "expensio.expense.events.v1.dlq"

	// billing 事件是独立主题：报表消费组不应该被订阅/账单变化唤醒，
	// 两套消费者的重试与积压也互不影响。
	TopicBilling    = "expensio.billing.events.v1"
	TopicBillingDLQ = "expensio.billing.events.v1.dlq"

	// SchemaVersion 是当前唯一支持的版本。不兼容变更必须升版本并换主题。
	SchemaVersion = 1

	// ConsumerReportExpense 是 report 的消费组名，幂等表按它区分消费者。
	ConsumerReportExpense = "report-expense-v1"

	// notification 有两类消费者：一个跟费用事件，一个跟账单事件。
	// 消费组名同时是 processed_events.consumer 的取值，改名会让幂等窗口重置。
	ConsumerNotificationExpense = "notification-expense-v1"
	ConsumerNotificationBilling = "notification-billing-v1"
)

// Type 是事件类型，取值与 outbox 的 CHECK 约束一致。
type Type string

const (
	TypeExpenseSubmitted  Type = "expense.submitted"
	TypeExpenseApproved   Type = "expense.approved"
	TypeExpenseRejected   Type = "expense.rejected"
	TypeExpenseCancelled  Type = "expense.cancelled"
	TypeExpenseReimbursed Type = "expense.reimbursed"

	TypeSubscriptionCreated  Type = "subscription.created"
	TypeSubscriptionCanceled Type = "subscription.canceled"
	TypeInvoiceCreated       Type = "invoice.created"
	TypeInvoicePaid          Type = "invoice.paid"
	TypeInvoicePaymentFailed Type = "invoice.payment_failed"
)

func (t Type) Valid() bool {
	switch t {
	case TypeExpenseSubmitted, TypeExpenseApproved, TypeExpenseRejected,
		TypeExpenseCancelled, TypeExpenseReimbursed,
		TypeSubscriptionCreated, TypeSubscriptionCanceled,
		TypeInvoiceCreated, TypeInvoicePaid, TypeInvoicePaymentFailed:
		return true
	default:
		return false
	}
}

// Envelope 是所有事件的统一信封。Payload 保留原始 JSON，
// 这样新增字段时旧消费者不会因为结构体不认识而丢事件。
type Envelope struct {
	EventID          uuid.UUID       `json:"event_id"`
	Type             Type            `json:"event_type"`
	SchemaVersion    int             `json:"schema_version"`
	TenantID         uuid.UUID       `json:"tenant_id"`
	AggregateID      uuid.UUID       `json:"aggregate_id"`
	AggregateVersion int             `json:"aggregate_version"`
	OccurredAt       time.Time       `json:"occurred_at"`
	Payload          json.RawMessage `json:"payload"`
}

// PublishKey 返回 Kafka 分区键：同一租户的事件必须落在同一分区，
// 否则同一张单的 submitted 与 approved 可能被乱序消费。
func (e Envelope) PublishKey() string { return e.TenantID.String() }

// Validate 检查信封的结构性约束，不解释业务字段。
func (e Envelope) Validate() error {
	var errs []error
	if e.EventID == uuid.Nil {
		errs = append(errs, errors.New("event_id 不能为空"))
	}
	if !e.Type.Valid() {
		errs = append(errs, fmt.Errorf("未知事件类型 %q", e.Type))
	}
	if e.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("不支持的 schema_version %d", e.SchemaVersion))
	}
	if e.TenantID == uuid.Nil {
		errs = append(errs, errors.New("tenant_id 不能为空"))
	}
	if e.AggregateID == uuid.Nil {
		errs = append(errs, errors.New("aggregate_id 不能为空"))
	}
	if e.AggregateVersion <= 0 {
		errs = append(errs, errors.New("aggregate_version 必须为正"))
	}
	if e.OccurredAt.IsZero() {
		errs = append(errs, errors.New("occurred_at 不能为空"))
	}
	if len(e.Payload) == 0 || !json.Valid(e.Payload) {
		errs = append(errs, errors.New("payload 必须是合法 JSON"))
	}
	return errors.Join(errs...)
}

// Encode 校验后序列化为完整信封，outbox 存的就是这份字节。
func (e Envelope) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("events: 信封非法: %w", err)
	}
	return json.Marshal(e)
}

// Decode 反序列化并校验。失败意味着消息格式不对，属于永久错误。
func Decode(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("events: 解码信封失败: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("events: 信封非法: %w", err)
	}
	return e, nil
}

// ExpenseSnapshot 是费用的全量快照，刻意不含商户名、描述与驳回理由：
// 报表只需要聚合维度，事件总线不承载非必要的文本数据。
type ExpenseSnapshot struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	SubmitterID uuid.UUID  `json:"submitter_id"`
	Status      string     `json:"status"`
	Category    string     `json:"category"`
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"`
	OccurredAt  time.Time  `json:"occurred_at"`
	BudgetID    *uuid.UUID `json:"budget_id,omitempty"`
	DecidedBy   *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	Version     int        `json:"version"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// 与 expense 数据库 CHECK 保持一致。事件是跨服务契约，消费端不能
// 依赖生产端的 Go 类型，因此这里保留一份显式白名单。
var validStatuses = map[string]struct{}{
	"draft": {}, "submitted": {}, "approved": {}, "rejected": {}, "cancelled": {}, "reimbursed": {},
}

var validCategories = map[string]struct{}{
	"travel": {}, "meals": {}, "office": {}, "software": {}, "other": {},
}

// Validate 检查快照的必填字段与取值范围。
func (s ExpenseSnapshot) Validate() error {
	var errs []error
	if s.ID == uuid.Nil || s.TenantID == uuid.Nil || s.SubmitterID == uuid.Nil {
		errs = append(errs, errors.New("id/tenant_id/submitter_id 不能为空"))
	}
	if _, ok := validStatuses[s.Status]; !ok {
		errs = append(errs, fmt.Errorf("非法状态 %q", s.Status))
	}
	if _, ok := validCategories[s.Category]; !ok {
		errs = append(errs, fmt.Errorf("非法类别 %q", s.Category))
	}
	if !validCurrency(s.Currency) {
		errs = append(errs, fmt.Errorf("币种必须是三位大写字母，当前为 %q", s.Currency))
	}
	if s.AmountMinor < 1 || s.AmountMinor > 1_000_000_000_000 {
		errs = append(errs, fmt.Errorf("金额超出范围: %d", s.AmountMinor))
	}
	if s.OccurredAt.IsZero() || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		errs = append(errs, errors.New("时间字段不能为空"))
	}
	if s.Version <= 0 {
		errs = append(errs, errors.New("version 必须为正"))
	}
	return errors.Join(errs...)
}

func validCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// SubscriptionSnapshot 是订阅的全量快照。
// 日期字段是 UTC 日粒度，与数据库的 date 列一致。
type SubscriptionSnapshot struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	PlanCode           string     `json:"plan_code"`
	Seats              int        `json:"seats"`
	Status             string     `json:"status"`
	CurrentPeriodStart time.Time  `json:"current_period_start"`
	CurrentPeriodEnd   time.Time  `json:"current_period_end"`
	Version            int        `json:"version"`
	CanceledAt         *time.Time `json:"canceled_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

var validSubscriptionStatuses = map[string]struct{}{
	"active": {}, "past_due": {}, "canceled": {},
}

func (s SubscriptionSnapshot) Validate() error {
	var errs []error
	if s.ID == uuid.Nil || s.TenantID == uuid.Nil {
		errs = append(errs, errors.New("id/tenant_id 不能为空"))
	}
	if s.PlanCode == "" {
		errs = append(errs, errors.New("plan_code 不能为空"))
	}
	if s.Seats < 1 || s.Seats > 1000 {
		errs = append(errs, fmt.Errorf("席位数超出范围: %d", s.Seats))
	}
	if _, ok := validSubscriptionStatuses[s.Status]; !ok {
		errs = append(errs, fmt.Errorf("非法订阅状态 %q", s.Status))
	}
	if s.CurrentPeriodStart.IsZero() || s.CurrentPeriodEnd.IsZero() ||
		s.CurrentPeriodEnd.Before(s.CurrentPeriodStart) {
		errs = append(errs, errors.New("订阅周期不合法"))
	}
	if (s.Status == "canceled") != (s.CanceledAt != nil) {
		errs = append(errs, errors.New("取消状态与取消时间必须同时出现"))
	}
	if s.Version <= 0 {
		errs = append(errs, errors.New("version 必须为正"))
	}
	return errors.Join(errs...)
}

// InvoiceSnapshot 是账单的全量快照。金额为最小货币单位。
type InvoiceSnapshot struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	Number         string     `json:"number"`
	Status         string     `json:"status"`
	PeriodStart    time.Time  `json:"period_start"`
	PeriodEnd      time.Time  `json:"period_end"`
	Currency       string     `json:"currency"`
	AmountMinor    int64      `json:"amount_minor"`
	DueOn          time.Time  `json:"due_on"`
	IssuedAt       time.Time  `json:"issued_at"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
	Version        int        `json:"version"`
}

func (s InvoiceSnapshot) Validate() error {
	var errs []error
	if s.ID == uuid.Nil || s.TenantID == uuid.Nil || s.SubscriptionID == uuid.Nil {
		errs = append(errs, errors.New("id/tenant_id/subscription_id 不能为空"))
	}
	if s.Number == "" {
		errs = append(errs, errors.New("账单号不能为空"))
	}
	if s.Status != "open" && s.Status != "paid" {
		errs = append(errs, fmt.Errorf("非法账单状态 %q", s.Status))
	}
	if !validCurrency(s.Currency) {
		errs = append(errs, fmt.Errorf("币种必须是三位大写字母，当前为 %q", s.Currency))
	}
	if s.AmountMinor < 1 || s.AmountMinor > 1_000_000_000_000 {
		errs = append(errs, fmt.Errorf("金额超出范围: %d", s.AmountMinor))
	}
	if s.PeriodStart.IsZero() || s.PeriodEnd.IsZero() || s.PeriodEnd.Before(s.PeriodStart) {
		errs = append(errs, errors.New("账单周期不合法"))
	}
	if s.DueOn.IsZero() || s.DueOn.Before(s.PeriodStart) {
		errs = append(errs, errors.New("到期日必须不早于周期开始"))
	}
	if (s.Status == "paid") != (s.PaidAt != nil) {
		errs = append(errs, errors.New("已支付状态与支付时间必须同时出现"))
	}
	if s.Version <= 0 {
		errs = append(errs, errors.New("version 必须为正"))
	}
	return errors.Join(errs...)
}
