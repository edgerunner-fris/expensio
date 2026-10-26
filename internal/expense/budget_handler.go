package expense

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

// budgetView 是预算的对外形态。期间保持 YYYY-MM-DD，
// 金额同时给出总额、占用、支出与可用额，前端不必自己算（也不该自己算）。
type budgetView struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Category       string    `json:"category"`
	Currency       string    `json:"currency"`
	PeriodStart    string    `json:"period_start"`
	PeriodEnd      string    `json:"period_end"`
	AmountMinor    int64     `json:"amount_minor"`
	ReservedMinor  int64     `json:"reserved_minor"`
	SpentMinor     int64     `json:"spent_minor"`
	AvailableMinor int64     `json:"available_minor"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func newBudgetView(b *Budget) budgetView {
	return budgetView{
		ID:             b.ID,
		Name:           b.Name,
		Category:       string(b.Category),
		Currency:       string(b.Currency),
		PeriodStart:    dateString(b.PeriodStart),
		PeriodEnd:      dateString(b.PeriodEnd),
		AmountMinor:    b.AmountMinor,
		ReservedMinor:  b.ReservedMinor,
		SpentMinor:     b.SpentMinor,
		AvailableMinor: b.AvailableMinor(),
		Version:        b.Version,
		CreatedAt:      b.CreatedAt,
		UpdatedAt:      b.UpdatedAt,
	}
}

func newBudgetViews(items []Budget) []budgetView {
	out := make([]budgetView, 0, len(items))
	for i := range items {
		out = append(out, newBudgetView(&items[i]))
	}
	return out
}

func budgetID(c *gin.Context) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param("budgetID"))
	if err != nil {
		return uuid.Nil, apperr.InvalidArgument("预算 ID 非法")
	}
	return id, nil
}

func (h *Handler) CreateBudget(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in BudgetInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	b, err := h.svc.CreateBudget(c.Request.Context(), actor, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, newBudgetView(b))
}

func (h *Handler) ListBudgets(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var q struct {
		Page     int    `form:"page"`
		PageSize int    `form:"page_size"`
		Category string `form:"category"`
	}
	if bindErr := c.ShouldBindQuery(&q); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}
	page := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize}
	page.Normalize()

	items, total, err := h.svc.ListBudgets(c.Request.Context(), actor, BudgetQuery{
		Category: q.Category,
		Limit:    page.PageSize,
		Offset:   page.Offset(),
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, httpx.NewPage(newBudgetViews(items), total, page))
}

func (h *Handler) GetBudget(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := budgetID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	b, err := h.svc.GetBudget(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newBudgetView(b))
}

func (h *Handler) UpdateBudget(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := budgetID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in BudgetUpdateInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	b, err := h.svc.UpdateBudget(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newBudgetView(b))
}

func (h *Handler) DeleteBudget(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := budgetID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	version, convErr := strconv.Atoi(c.Query("version"))
	if convErr != nil {
		httpx.Error(c, apperr.InvalidArgument("version 必须是正整数"))
		return
	}
	if err := h.svc.DeleteBudget(c.Request.Context(), actor, id, version); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
