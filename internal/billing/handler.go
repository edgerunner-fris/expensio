package billing

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Handler 只做请求解析与响应序列化；权限与事务规则在 service/领域层。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// actorFrom 从 JWT 派生的租户上下文取身份，请求体里没有操作人字段。
func actorFrom(ctx context.Context) (tenantctx.Tenant, error) {
	t, ok := tenantctx.From(ctx)
	if !ok {
		return tenantctx.Tenant{}, apperr.Unauthorized("缺少访问令牌")
	}
	return t, nil
}

type planView struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
	Interval   string `json:"interval"`
}

func newPlanView(p Plan) planView {
	return planView{
		Code:       p.Code,
		Name:       p.Name,
		PriceMinor: p.PriceMinor,
		Currency:   string(p.Currency),
		Interval:   string(p.Interval),
	}
}

// subscriptionView 的日期保持 YYYY-MM-DD：计费周期是日期概念，
// 带时区的时间戳会让"10 月 31 日"在前端变成"10 月 30 日"。
type subscriptionView struct {
	ID                 uuid.UUID  `json:"id"`
	PlanCode           string     `json:"plan_code"`
	Seats              int        `json:"seats"`
	Status             string     `json:"status"`
	CurrentPeriodStart string     `json:"current_period_start"`
	CurrentPeriodEnd   string     `json:"current_period_end"`
	Version            int        `json:"version"`
	CanceledAt         *time.Time `json:"canceled_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func newSubscriptionView(s *Subscription) subscriptionView {
	return subscriptionView{
		ID:                 s.ID,
		PlanCode:           s.PlanCode,
		Seats:              s.Seats,
		Status:             string(s.Status),
		CurrentPeriodStart: dateString(s.CurrentPeriodStart),
		CurrentPeriodEnd:   dateString(s.CurrentPeriodEnd),
		Version:            s.Version,
		CanceledAt:         s.CanceledAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
}

func newSubscriptionViews(items []Subscription) []subscriptionView {
	out := make([]subscriptionView, 0, len(items))
	for i := range items {
		out = append(out, newSubscriptionView(&items[i]))
	}
	return out
}

func subscriptionID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("subscriptionID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidArgument("订阅 ID 非法")
	}
	return id, nil
}

func (h *Handler) ListPlans(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	plans, err := h.svc.ListPlans(c.Request.Context(), actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	views := make([]planView, 0, len(plans))
	for _, p := range plans {
		views = append(views, newPlanView(p))
	}
	httpx.OK(c, views)
}

func (h *Handler) CreateSubscription(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in SubscriptionInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	sub, err := h.svc.CreateSubscription(c.Request.Context(), actor, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, newSubscriptionView(sub))
}

func (h *Handler) GetCurrentSubscription(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	sub, err := h.svc.GetCurrentSubscription(c.Request.Context(), actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newSubscriptionView(sub))
}

func (h *Handler) GetSubscription(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := subscriptionID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	sub, err := h.svc.GetSubscription(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newSubscriptionView(sub))
}

func (h *Handler) ListSubscriptions(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var q struct {
		Page     int `form:"page"`
		PageSize int `form:"page_size"`
	}
	if bindErr := c.ShouldBindQuery(&q); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}
	page := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize}
	page.Normalize()

	items, total, err := h.svc.ListSubscriptions(c.Request.Context(), actor, page.PageSize, page.Offset())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, httpx.NewPage(newSubscriptionViews(items), total, page))
}

func (h *Handler) CancelSubscription(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := subscriptionID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in struct {
		Version int `json:"version"`
	}
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	sub, err := h.svc.CancelSubscription(c.Request.Context(), actor, id, in.Version)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newSubscriptionView(sub))
}

// invoiceView 的日期保持 YYYY-MM-DD，时间戳保留 RFC3339。
type invoiceView struct {
	ID             uuid.UUID  `json:"id"`
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	Number         string     `json:"number"`
	Status         string     `json:"status"`
	PeriodStart    string     `json:"period_start"`
	PeriodEnd      string     `json:"period_end"`
	Currency       string     `json:"currency"`
	AmountMinor    int64      `json:"amount_minor"`
	DueOn          string     `json:"due_on"`
	IssuedAt       time.Time  `json:"issued_at"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
	Version        int        `json:"version"`
}

func newInvoiceView(inv *Invoice) invoiceView {
	return invoiceView{
		ID:             inv.ID,
		SubscriptionID: inv.SubscriptionID,
		Number:         inv.Number,
		Status:         string(inv.Status),
		PeriodStart:    dateString(inv.PeriodStart),
		PeriodEnd:      dateString(inv.PeriodEnd),
		Currency:       string(inv.Currency),
		AmountMinor:    inv.AmountMinor,
		DueOn:          dateString(inv.DueOn),
		IssuedAt:       inv.IssuedAt,
		PaidAt:         inv.PaidAt,
		Version:        inv.Version,
	}
}

func invoiceID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("invoiceID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidArgument("账单 ID 非法")
	}
	return id, nil
}

func (h *Handler) GenerateInvoice(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := subscriptionID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	inv, err := h.svc.GenerateInvoice(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, newInvoiceView(inv))
}

func (h *Handler) GetInvoice(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := invoiceID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	inv, err := h.svc.GetInvoice(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newInvoiceView(inv))
}

func (h *Handler) ListInvoices(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var q struct {
		Page     int    `form:"page"`
		PageSize int    `form:"page_size"`
		Status   string `form:"status"`
	}
	if bindErr := c.ShouldBindQuery(&q); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}
	page := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize}
	page.Normalize()

	items, total, err := h.svc.ListInvoices(c.Request.Context(), actor, InvoiceQuery{
		Status: q.Status,
		Limit:  page.PageSize,
		Offset: page.Offset(),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	views := make([]invoiceView, 0, len(items))
	for i := range items {
		views = append(views, newInvoiceView(&items[i]))
	}
	httpx.OK(c, httpx.NewPage(views, total, page))
}

// PaymentWebhook 是支付渠道的入站回调：没有 JWT，信任完全来自签名。
// 原始字节直接交给 service 验签，handler 不做任何状态判断。
func (h *Handler) PaymentWebhook(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBody)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		httpx.Error(c, apperr.InvalidArgument("回调请求体读取失败"))
		return
	}
	res, err := h.svc.HandlePaymentWebhook(c.Request.Context(), PaymentWebhookInput{
		Provider:  c.GetHeader("X-Billing-Provider"),
		Timestamp: c.GetHeader("X-Billing-Timestamp"),
		Signature: c.GetHeader("X-Billing-Signature"),
		Body:      raw,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	status := "processed"
	if res.Duplicate {
		status = "duplicate"
	}
	httpx.OK(c, struct {
		Status    string    `json:"status"`
		EventType string    `json:"event_type"`
		InvoiceID uuid.UUID `json:"invoice_id"`
		Duplicate bool      `json:"duplicate"`
	}{
		Status:    status,
		EventType: string(res.EventType),
		InvoiceID: res.InvoiceID,
		Duplicate: res.Duplicate,
	})
}
