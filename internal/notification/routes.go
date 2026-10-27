package notification

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

// RegisterRoutes 注册收件箱接口。所有端点都要求用户 access token：
// 通知按用户隔离，没有任何匿名可读的通知。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	// 服务间接口：租户由签名令牌声明，网关不转发这个前缀。
	v1.POST("/internal/emails", h.InternalEmail)

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))
	authed.GET("/notifications", h.List)
	authed.GET("/notifications/unread-count", h.UnreadCount)
	authed.POST("/notifications/read-all", h.MarkAllRead)
	authed.POST("/notifications/:notificationID/read", h.MarkRead)
}
