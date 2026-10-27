package notification

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
)

// Handler 只解析请求与写响应。
type Handler struct {
	svc *Service
	// internalToken 只用于 /internal/* 接口，与用户 JWT 密钥分开配置。
	internalToken string
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// EnableInternalAPI 注入服务间令牌密钥。空字符串时 internal 接口一律拒绝。
func (h *Handler) EnableInternalAPI(token string) { h.internalToken = token }

// internalEmailRequest 是 identity 投递邀请邮件时使用的请求体。
// 租户不在 body 里：它只能来自签名令牌，客户端无法指定替谁发信。
type internalEmailRequest struct {
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	DedupeKey string `json:"dedupe_key"`
}

// InternalEmail 把一封邮件写入发件箱。它只做入队，不连 SMTP：
// 投递由后台 worker 完成，调用方不会因为 SMTP 慢而超时。
func (h *Handler) InternalEmail(c *gin.Context) {
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	tenantID, err := serviceauth.Verify(h.internalToken, raw, time.Now())
	if err != nil {
		httpx.Error(c, apperr.Unauthorized("内部令牌无效"))
		return
	}

	var in internalEmailRequest
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	err = h.svc.EnqueueEmail(c.Request.Context(), tenantID, Outbound{
		Recipient: in.Recipient,
		Subject:   in.Subject,
		Body:      in.Body,
		DedupeKey: in.DedupeKey,
	})
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, gin.H{"queued": true})
}

func (h *Handler) List(c *gin.Context) {
	var q listQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("分页参数非法"))
		return
	}
	page, err := h.svc.List(c.Request.Context(), q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, page)
}

func (h *Handler) UnreadCount(c *gin.Context) {
	count, err := h.svc.UnreadCount(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, gin.H{"unread": count})
}

func (h *Handler) MarkRead(c *gin.Context) {
	id, err := uuid.Parse(c.Param("notificationID"))
	if err != nil {
		httpx.Error(c, apperr.InvalidArgument("通知 ID 非法"))
		return
	}
	if err := h.svc.MarkRead(c.Request.Context(), id); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}

func (h *Handler) MarkAllRead(c *gin.Context) {
	affected, err := h.svc.MarkAllRead(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, gin.H{"updated": affected})
}
