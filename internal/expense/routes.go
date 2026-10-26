package expense

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

// RegisterRoutes 注册 expense 的对外端点。
//
// 所有端点都先过 Auth：expense 不假设网关已经校验过令牌。
// 角色与所有权规则在领域层（AuthorizeDraftWrite / CanView），
// 这里只用中间件表达"必须是已认证用户"，不重复角色判断。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))

	authed.POST("/expenses", h.CreateExpense)
	authed.GET("/expenses", h.ListExpenses)
	authed.GET("/expenses/:expenseID", h.GetExpense)
	authed.PUT("/expenses/:expenseID", h.UpdateExpense)
	authed.DELETE("/expenses/:expenseID", h.DeleteExpense)
}
