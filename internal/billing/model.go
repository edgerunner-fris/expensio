// Package billing 实现订阅计费：套餐、订阅、出账与支付回调。
//
// 本文件只放与数据库无关的领域规则，方便用纯单元测试固定：
// 状态机、金额计算、套餐校验。仓储与 HTTP 层在后续批次接入。
// 设计见 docs/adr/0005-billing-subscriptions-and-payment-callbacks.md。
package billing

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	// MinSeats / MaxSeats 与数据库 CHECK (seats BETWEEN 1 AND 1000) 一致。
	MinSeats = 1
	MaxSeats = 1000

	maxPlanNameRunes = 80
	minPlanCodeLen   = 2
	maxPlanCodeLen   = 41 // 与 plans_code_check 的 {1,40} 尾部对应
)

// PlanInterval 是套餐计费周期。
type PlanInterval string

const (
	IntervalMonthly PlanInterval = "monthly"
	IntervalYearly  PlanInterval = "yearly"
)

func (i PlanInterval) Valid() bool {
	return i == IntervalMonthly || i == IntervalYearly
}

// Plan 是全局套餐目录的一条记录。价格按"每席位每周期"计。
type Plan struct {
	Code       string
	Name       string
	PriceMinor int64
	Currency   money.Currency
	Interval   PlanInterval
}

// Validate 校验套餐是否可用于出账。
// 目录本身由迁移种子化且运行时只读，这里的校验主要防御扫描/装配错误。
func (p Plan) Validate() error {
	if !validPlanCode(p.Code) {
		return apperr.InvalidArgument("套餐代码必须是小写字母开头的 2-41 位小写字母、数字或连字符")
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || utf8.RuneCountInString(name) > maxPlanNameRunes {
		return apperr.InvalidArgument("套餐名称不能为空且不超过 80 个字符")
	}
	if p.PriceMinor < money.MinMinor || p.PriceMinor > money.MaxMinor {
		return apperr.InvalidArgument("套餐单价超出允许范围")
	}
	if !p.Currency.Valid() {
		return apperr.InvalidArgument("套餐币种必须是三位大写字母")
	}
	if !p.Interval.Valid() {
		return apperr.InvalidArgument("套餐周期必须是 monthly 或 yearly")
	}
	return nil
}

// validPlanCode 与数据库 plans_code_check 保持同一规则。
func validPlanCode(code string) bool {
	if len(code) < minPlanCodeLen || len(code) > maxPlanCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// SubscriptionStatus 是订阅状态。
type SubscriptionStatus string

const (
	SubscriptionActive   SubscriptionStatus = "active"
	SubscriptionPastDue  SubscriptionStatus = "past_due"
	SubscriptionCanceled SubscriptionStatus = "canceled"
)

func (s SubscriptionStatus) Valid() bool {
	switch s {
	case SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled:
		return true
	default:
		return false
	}
}

// subscriptionTransitions 与迁移里的 subscription_status_transitions 一致。
// 两处都写是有意的：数据库拦截手工修数，Go 侧让人在写代码时就看到白名单。
var subscriptionTransitions = map[SubscriptionStatus]map[SubscriptionStatus]struct{}{
	SubscriptionActive: {
		SubscriptionPastDue:  {},
		SubscriptionCanceled: {},
	},
	SubscriptionPastDue: {
		SubscriptionActive:   {},
		SubscriptionCanceled: {},
	},
	SubscriptionCanceled: {},
}

// CanTransition 判断订阅状态边是否合法。canceled 是终态。
func (s SubscriptionStatus) CanTransition(to SubscriptionStatus) bool {
	if !s.Valid() || !to.Valid() {
		return false
	}
	_, ok := subscriptionTransitions[s][to]
	return ok
}

// InvoiceStatus 是账单状态。M4 只有 open → paid，没有作废流程。
type InvoiceStatus string

const (
	InvoiceOpen InvoiceStatus = "open"
	InvoicePaid InvoiceStatus = "paid"
)

func (s InvoiceStatus) Valid() bool {
	return s == InvoiceOpen || s == InvoicePaid
}

// CanTransition 判断账单状态边是否合法。paid 是终态。
func (s InvoiceStatus) CanTransition(to InvoiceStatus) bool {
	return s == InvoiceOpen && to == InvoicePaid
}

// WebhookEventType 是支付渠道回调的事件类型。
type WebhookEventType string

const (
	WebhookPaymentSucceeded WebhookEventType = "payment.succeeded"
	WebhookPaymentFailed    WebhookEventType = "payment.failed"
)

func (t WebhookEventType) Valid() bool {
	return t == WebhookPaymentSucceeded || t == WebhookPaymentFailed
}

// Subscription 是一条订阅的领域视图。日期字段是 UTC 的日粒度。
type Subscription struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	PlanCode           string
	Seats              int
	Status             SubscriptionStatus
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	Version            int
	CanceledAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CreatedBy          uuid.UUID
	UpdatedBy          uuid.UUID
}

// Invoice 是一张账单的领域视图。
type Invoice struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	SubscriptionID uuid.UUID
	Number         string
	Status         InvoiceStatus
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Currency       money.Currency
	AmountMinor    int64
	DueOn          time.Time
	IssuedAt       time.Time
	PaidAt         *time.Time
	Version        int
}

// InvoiceAmount 计算一个周期的账单金额：套餐单价 × 席位。
//
// 先做除法再比较，避免 price * seats 在 int64 上溢出后变成一个"合法"的负数。
func InvoiceAmount(plan Plan, seats int) (int64, error) {
	if err := plan.Validate(); err != nil {
		return 0, err
	}
	if seats < MinSeats || seats > MaxSeats {
		return 0, apperr.InvalidArgument("席位数必须在 1 到 1000 之间")
	}
	if plan.PriceMinor > money.MaxMinor/int64(seats) {
		return 0, apperr.InvalidArgument("账单金额超出允许范围")
	}
	return plan.PriceMinor * int64(seats), nil
}

// CanManageBilling 限定订阅与出账的写操作只能由 owner/admin 执行。
// 账单本身属于租户，成员与查看者可以读取，但不能改动计费。
func CanManageBilling(role tenantctx.Role) bool {
	return role.CanManageMembers()
}
