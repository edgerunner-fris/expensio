package identity

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

type switchTenantRequest struct {
	TenantID string `json:"tenant_id"`
}

// Tenants 返回当前账号的全部成员关系。
func (h *Handler) Tenants(c *gin.Context) {
	items, err := h.svc.Tenants(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}

// SwitchTenant 换取目标租户的会话。目标租户只从请求体读取 ID，
// 角色与状态一律由服务端查询，客户端无法声明。
func (h *Handler) SwitchTenant(c *gin.Context) {
	var in switchTenantRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	targetID, err := uuid.Parse(in.TenantID)
	if err != nil {
		httpx.Error(c, apperr.InvalidArgument("租户 ID 非法"))
		return
	}
	session, err := h.svc.SwitchTenant(c.Request.Context(), targetID, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, session)
}

// UpdateProfile 修改展示名。
func (h *Handler) UpdateProfile(c *gin.Context) {
	var in UpdateProfileInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	profile, err := h.svc.UpdateProfile(c.Request.Context(), in, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, profile)
}

// ChangePassword 修改密码；成功后该账号的全部刷新令牌失效。
func (h *Handler) ChangePassword(c *gin.Context) {
	var in ChangePasswordInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	if err := h.svc.ChangePassword(c.Request.Context(), in, requestMeta(c)); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
