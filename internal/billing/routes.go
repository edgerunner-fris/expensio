package billing

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// RegisterRoutes 注册 billing 的对外端点。
//
// 套餐与订阅对全租户可读；开通/取消订阅只允许 owner/admin，
// 网关、RequireRole 与 service 各挡一次，任一层的漏判都不会放行越权写。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))

	authed.GET("/billing/plans", h.ListPlans)
	authed.GET("/billing/subscription", h.GetCurrentSubscription)
	authed.GET("/billing/subscriptions", h.ListSubscriptions)
	authed.GET("/billing/subscriptions/:subscriptionID", h.GetSubscription)

	admin := authed.Group("")
	admin.Use(middleware.RequireRole(tenantctx.RoleAdmin))
	admin.POST("/billing/subscriptions", h.CreateSubscription)
	admin.POST("/billing/subscriptions/:subscriptionID/cancel", h.CancelSubscription)
}
