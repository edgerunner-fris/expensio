package identity

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// RegisterRoutes 注册身份服务的全部对外端点。
//
// 未认证的只有四个：注册、登录、刷新、登出——它们拿到的是"凭据"而不是"会话"，
// 因此不能要求 access token；其中刷新与登出入口本身就是凭据。
// 其余端点一律先过 Auth：identity 服务不假设网关已经校验过。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	v1.POST("/auth/register", h.Register)
	v1.POST("/auth/login", h.Login)
	v1.POST("/auth/refresh", h.Refresh)
	v1.POST("/auth/logout", h.Logout)

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))
	authed.GET("/me", h.Me)

	// 成员与审计属于管理视图：普通成员能看到同事邮箱、管理员能看到全租户操作记录，
	// 这些都不是 member / viewer 该拿到的数据。
	admin := authed.Group("")
	admin.Use(middleware.RequireRole(tenantctx.RoleAdmin))
	admin.GET("/tenants/current/members", h.ListMembers)
	admin.PATCH("/tenants/current/members/:userID", h.UpdateMemberRole)
	admin.GET("/audit-logs", h.ListAuditLogs)
}
