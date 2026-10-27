package identity

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// TokenSource 是 identity 对令牌能力的完整依赖：既要校验 access token，
// 也要把自己的公钥发布成 JWKS 供第三方验签。
type TokenSource interface {
	middleware.AccessParser
	JWKS() auth.JWKS
}

// RegisterRoutes 注册身份服务的全部对外端点。
//
// 未认证的只有五个：注册、登录、刷新、登出、JWKS——前四个拿到的是"凭据"
// 而不是"会话"；JWKS 只含公钥，本身就是公开信息。
// 其余端点一律先过 Auth：identity 服务不假设网关已经校验过。
func RegisterRoutes(r gin.IRouter, h *Handler, tokens TokenSource) {
	// RFC 8414 位置的 JWKS：轮换后验签方按 kid 取新公钥，不需要重新分发密钥。
	r.GET("/.well-known/jwks.json", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=300")
		c.JSON(http.StatusOK, tokens.JWKS())
	})

	v1 := r.Group("/api/v1")

	v1.POST("/auth/register", h.Register)
	v1.POST("/auth/login", h.Login)
	v1.POST("/auth/refresh", h.Refresh)
	v1.POST("/auth/logout", h.Logout)

	// 服务间接口：租户由签名令牌声明，不由客户端选择。网关不转发这个前缀。
	v1.GET("/internal/tenant-contacts", h.TenantContacts)

	// 邀请接受页是公开的：被邀请人还没有账号，不可能先登录。
	// 令牌本身是凭据；网关对这条前缀按 IP 限流。
	v1.GET("/invitations/:token", h.InvitationInfo)
	v1.POST("/invitations/:token/accept", h.AcceptInvitation)

	authed := v1.Group("")
	authed.Use(middleware.Auth(tokens))
	authed.GET("/me", h.Me)

	// 成员与审计属于管理视图：普通成员能看到同事邮箱、管理员能看到全租户操作记录，
	// 这些都不是 member / viewer 该拿到的数据。
	admin := authed.Group("")
	admin.Use(middleware.RequireRole(tenantctx.RoleAdmin))
	admin.GET("/tenants/current/members", h.ListMembers)
	admin.PATCH("/tenants/current/members/:userID", h.UpdateMemberRole)
	admin.GET("/tenants/current/invitations", h.ListInvitations)
	admin.POST("/tenants/current/invitations", h.CreateInvitation)
	admin.DELETE("/tenants/current/invitations/:invitationID", h.RevokeInvitation)
	admin.GET("/audit-logs", h.ListAuditLogs)
}
