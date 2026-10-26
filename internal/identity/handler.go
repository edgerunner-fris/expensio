package identity

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// maxUserAgentLen 限制写入数据库与审计的 UA 长度。
// 请求头是客户端可控输入，不设上限等于让任何人往我们的表里塞任意长度的字符串。
const maxUserAgentLen = 512

// Handler 只做三件事：解析请求、调用 service、写响应。
// 任何业务判断都不应该出现在这里，否则同一规则会在网关和前端各被复制一遍。
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(c *gin.Context) {
	var in RegisterInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	session, err := h.svc.Register(c.Request.Context(), in, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, session)
}

func (h *Handler) Login(c *gin.Context) {
	var in LoginInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	session, err := h.svc.Login(c.Request.Context(), in, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, session)
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) Refresh(c *gin.Context) {
	var in refreshRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	session, err := h.svc.Refresh(c.Request.Context(), in.RefreshToken, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, session)
}

func (h *Handler) Logout(c *gin.Context) {
	var in refreshRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	if err := h.svc.Logout(c.Request.Context(), in.RefreshToken, requestMeta(c)); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}

func (h *Handler) Me(c *gin.Context) {
	profile, err := h.svc.Me(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, profile)
}

func (h *Handler) ListMembers(c *gin.Context) {
	var q httpx.PageQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("分页参数非法"))
		return
	}
	page, err := h.svc.ListMembers(c.Request.Context(), q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, page)
}

type updateMemberRequest struct {
	Role string `json:"role"`
}

func (h *Handler) UpdateMemberRole(c *gin.Context) {
	targetID, err := uuid.Parse(c.Param("userID"))
	if err != nil {
		httpx.Error(c, apperr.InvalidArgument("用户 ID 非法"))
		return
	}
	var in updateMemberRequest
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	member, err := h.svc.UpdateMemberRole(c.Request.Context(), targetID, tenantctx.Role(in.Role), requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, member)
}

func (h *Handler) ListAuditLogs(c *gin.Context) {
	var q httpx.PageQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("分页参数非法"))
		return
	}
	page, err := h.svc.ListAuditLogs(c.Request.Context(), q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, page)
}

// requestMeta 采集请求来源。IP 来自 ClientIP（已关闭信任代理），
// UA 截断后入库，避免客户端用超长头把审计表撑大。
func requestMeta(c *gin.Context) RequestMeta {
	ua := c.GetHeader("User-Agent")
	if len(ua) > maxUserAgentLen {
		ua = ua[:maxUserAgentLen]
	}
	return RequestMeta{IP: c.ClientIP(), UserAgent: ua}
}
