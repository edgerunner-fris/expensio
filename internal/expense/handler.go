package expense

import (
	"context"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Handler 只做三件事：解析请求、调用 service、写响应。
// 权限判断在领域层，租户解析在中间件，handler 不重复任何一条规则。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// actorFrom 从 JWT 派生的租户上下文取身份。
// 请求体与查询参数里没有 actor 字段，因此调用方无法伪造操作人。
func actorFrom(ctx context.Context) (Actor, error) {
	t, ok := tenantctx.From(ctx)
	if !ok {
		return Actor{}, apperr.Unauthorized("缺少访问令牌")
	}
	return Actor{UserID: t.UserID, Role: t.Role}, nil
}

// expenseView 是费用单的对外形态。
// 与领域结构分开：领域字段变化（例如内部标记位）不应该自动出现在 API 契约里。
type expenseView struct {
	ID              uuid.UUID  `json:"id"`
	SubmitterID     uuid.UUID  `json:"submitter_id"`
	Status          string     `json:"status"`
	Category        string     `json:"category"`
	Currency        string     `json:"currency"`
	AmountMinor     int64      `json:"amount_minor"`
	Merchant        string     `json:"merchant"`
	Description     string     `json:"description"`
	OccurredAt      time.Time  `json:"occurred_at"`
	BudgetID        *uuid.UUID `json:"budget_id,omitempty"`
	DecidedBy       *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	RejectionReason string     `json:"rejection_reason,omitempty"`
	Version         int        `json:"version"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func newExpenseView(e *Expense) expenseView {
	return expenseView{
		ID:              e.ID,
		SubmitterID:     e.SubmitterID,
		Status:          string(e.Status),
		Category:        string(e.Category),
		Currency:        string(e.Currency),
		AmountMinor:     e.AmountMinor,
		Merchant:        e.Merchant,
		Description:     e.Description,
		OccurredAt:      e.OccurredAt,
		BudgetID:        e.BudgetID,
		DecidedBy:       e.DecidedBy,
		DecidedAt:       e.DecidedAt,
		RejectionReason: e.RejectionReason,
		Version:         e.Version,
		CreatedAt:       e.CreatedAt,
		UpdatedAt:       e.UpdatedAt,
	}
}

func newExpenseViews(items []Expense) []expenseView {
	out := make([]expenseView, 0, len(items))
	for i := range items {
		out = append(out, newExpenseView(&items[i]))
	}
	return out
}

func expenseID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("expenseID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidArgument("费用单 ID 非法")
	}
	return id, nil
}

func (h *Handler) CreateExpense(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in DraftInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	e, err := h.svc.CreateDraft(c.Request.Context(), actor, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, newExpenseView(e))
}

func (h *Handler) ListExpenses(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var q struct {
		Page     int    `form:"page"`
		PageSize int    `form:"page_size"`
		Scope    string `form:"scope"`
		Status   string `form:"status"`
	}
	if bindErr := c.ShouldBindQuery(&q); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}
	page := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize}
	page.Normalize()

	items, total, err := h.svc.List(c.Request.Context(), actor, ListQuery{
		Scope:  q.Scope,
		Status: q.Status,
		Limit:  page.PageSize,
		Offset: page.Offset(),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, httpx.NewPage(newExpenseViews(items), total, page))
}

func (h *Handler) GetExpense(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	e, err := h.svc.Get(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newExpenseView(e))
}

// updateExpenseRequest 把乐观锁版本号和可编辑字段放在同一个请求体里。
// 版本号必须由客户端回传：只按 ID 更新等于让最后一次写入静默获胜。
type updateExpenseRequest struct {
	Version int `json:"version"`
	DraftInput
}

func (h *Handler) UpdateExpense(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in updateExpenseRequest
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	e, err := h.svc.UpdateDraft(c.Request.Context(), actor, id, in.Version, in.DraftInput)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newExpenseView(e))
}

func (h *Handler) DeleteExpense(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	version, convErr := strconv.Atoi(c.Query("version"))
	if convErr != nil {
		httpx.Error(c, apperr.InvalidArgument("version 必须是正整数"))
		return
	}
	if err := h.svc.DeleteDraft(c.Request.Context(), actor, id, version); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
