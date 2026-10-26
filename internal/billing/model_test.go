package billing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func validPlan() Plan {
	return Plan{
		Code:       "growth-monthly",
		Name:       "Growth",
		PriceMinor: 9900,
		Currency:   money.Currency("USD"),
		Interval:   IntervalMonthly,
	}
}

func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	e, ok := apperr.As(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, apperr.CodeInvalidArgument, e.Code())
}

func TestPlanValidate(t *testing.T) {
	require.NoError(t, validPlan().Validate())

	tests := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"代码为空", func(p *Plan) { p.Code = "" }},
		{"代码以数字开头", func(p *Plan) { p.Code = "1plan" }},
		{"代码含大写", func(p *Plan) { p.Code = "Growth" }},
		{"代码含下划线", func(p *Plan) { p.Code = "growth_plan" }},
		{"代码过长", func(p *Plan) { p.Code = "a" + repeatA(41) }},
		{"名称为空", func(p *Plan) { p.Name = "   " }},
		{"名称过长", func(p *Plan) { p.Name = repeatA(81) }},
		{"单价为零", func(p *Plan) { p.PriceMinor = 0 }},
		{"单价为负", func(p *Plan) { p.PriceMinor = -1 }},
		{"单价超上限", func(p *Plan) { p.PriceMinor = money.MaxMinor + 1 }},
		{"币种小写", func(p *Plan) { p.Currency = "usd" }},
		{"周期非法", func(p *Plan) { p.Interval = "weekly" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPlan()
			tt.mutate(&p)
			requireInvalidArgument(t, p.Validate())
		})
	}
}

// repeatA 返回 n 个 'a'，让长度类用例不必手写长串。
func repeatA(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestSubscriptionTransitions(t *testing.T) {
	all := []SubscriptionStatus{SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled}
	allowed := map[[2]SubscriptionStatus]bool{
		{SubscriptionActive, SubscriptionPastDue}:   true,
		{SubscriptionActive, SubscriptionCanceled}:  true,
		{SubscriptionPastDue, SubscriptionActive}:   true,
		{SubscriptionPastDue, SubscriptionCanceled}: true,
	}

	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]SubscriptionStatus{from, to}]
			assert.Equal(t, want, from.CanTransition(to), "%s -> %s", from, to)
		}
	}
	assert.False(t, SubscriptionStatus("paused").Valid())
	assert.False(t, SubscriptionStatus("").CanTransition(SubscriptionActive))
}

func TestInvoiceTransitions(t *testing.T) {
	assert.True(t, InvoiceOpen.CanTransition(InvoicePaid))
	assert.False(t, InvoicePaid.CanTransition(InvoiceOpen))
	assert.False(t, InvoiceOpen.CanTransition(InvoiceOpen))
	assert.False(t, InvoiceStatus("void").Valid())
}

func TestWebhookEventType(t *testing.T) {
	assert.True(t, WebhookPaymentSucceeded.Valid())
	assert.True(t, WebhookPaymentFailed.Valid())
	assert.False(t, WebhookEventType("payment.refunded").Valid())
}

func TestInvoiceAmount(t *testing.T) {
	amount, err := InvoiceAmount(validPlan(), 5)
	require.NoError(t, err)
	assert.Equal(t, int64(49500), amount)

	_, err = InvoiceAmount(validPlan(), 0)
	requireInvalidArgument(t, err)
	_, err = InvoiceAmount(validPlan(), MaxSeats+1)
	requireInvalidArgument(t, err)

	// 乘法溢出必须被拦在 int64 回绕之前。
	overflow := validPlan()
	overflow.PriceMinor = money.MaxMinor
	_, err = InvoiceAmount(overflow, 2)
	requireInvalidArgument(t, err)

	// 恰好等于上限允许。
	boundary := validPlan()
	boundary.PriceMinor = money.MaxMinor / 2
	amount, err = InvoiceAmount(boundary, 2)
	require.NoError(t, err)
	assert.Equal(t, money.MaxMinor, amount)

	invalid := validPlan()
	invalid.PriceMinor = 0
	_, err = InvoiceAmount(invalid, 1)
	requireInvalidArgument(t, err)
}

func TestCanManageBilling(t *testing.T) {
	assert.True(t, CanManageBilling(tenantctx.RoleOwner))
	assert.True(t, CanManageBilling(tenantctx.RoleAdmin))
	assert.False(t, CanManageBilling(tenantctx.RoleMember))
	assert.False(t, CanManageBilling(tenantctx.RoleViewer))
	assert.False(t, CanManageBilling(tenantctx.Role("")))
}
