package report

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

// RegisterRoutes 注册报表端点。
//
// 路径必须与网关的前缀规则一致（/api/v1/reports → report 服务），
// 否则网关转发过来的请求在这里会 404。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))

	authed.GET("/reports/expenses/summary", h.ExpenseSummary)
}
