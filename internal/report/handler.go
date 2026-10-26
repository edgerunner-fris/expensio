package report

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Handler 只做解析与渲染。可见范围由 resolveScope 决定，
// 租户上下文只来自中间件校验过的 JWT。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ExpenseSummary 处理 GET /api/v1/reports/expenses/summary。
func (h *Handler) ExpenseSummary(c *gin.Context) {
	actor, ok := tenantctx.From(c.Request.Context())
	if !ok {
		httpx.Error(c, apperr.Unauthorized("缺少访问令牌"))
		return
	}

	var q SummaryQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}

	summary, err := h.svc.Summary(c.Request.Context(), actor, q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, summary)
}
