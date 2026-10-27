package identity

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// maxUserAgentLen 限制写入数据库与审计的 UA 长度。
// 请求头是客户端可控输入，不设上限等于让任何人往我们的表里塞任意长度的字符串。
const maxUserAgentLen = 512

// Handler 只做三件事：解析请求、调用 service、写响应。
// 任何业务判断都不应该出现在这里，否则同一规则会在网关和前端各被复制一遍。
type Handler struct {
	svc *Service
	// internalToken 只用于 /internal/* 接口，与用户 JWT 密钥分开配置。
	internalToken string
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// EnableInternalAPI 注入服务间令牌密钥。空字符串时 internal 接口一律拒绝。
func (h *Handler) EnableInternalAPI(token string) { h.internalToken = token }

// TenantContacts 是服务间接口：租户来自签名令牌而不是 query/body/path。
// 网关没有 /api/v1/internal 的转发规则，这个端点只应被集群内的
// notification 服务调用；身份验证失败时不区分"格式错"与"过期"，避免探测。
func (h *Handler) TenantContacts(c *gin.Context) {
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	tenantID, err := serviceauth.Verify(h.internalToken, raw, time.Now())
	if err != nil {
		httpx.Error(c, apperr.Unauthorized("内部令牌无效"))
		return
	}
	contacts, err := h.svc.TenantContacts(c.Request.Context(), tenantID)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, contacts)
}

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

func (h *Handler) CreateInvitation(c *gin.Context) {
	var in CreateInvitationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	out, err := h.svc.CreateInvitation(c.Request.Context(), in, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, out)
}

func (h *Handler) ListInvitations(c *gin.Context) {
	var q httpx.PageQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("分页参数非法"))
		return
	}
	page, err := h.svc.ListInvitations(c.Request.Context(), q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, page)
}

func (h *Handler) RevokeInvitation(c *gin.Context) {
	id, err := uuid.Parse(c.Param("invitationID"))
	if err != nil {
		httpx.Error(c, apperr.InvalidArgument("邀请 ID 非法"))
		return
	}
	if err := h.svc.RevokeInvitation(c.Request.Context(), id, requestMeta(c)); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}

// InvitationInfo 与 AcceptInvitation 是公开端点：
// 链接里的令牌就是凭据，不要求用户先登录。
func (h *Handler) InvitationInfo(c *gin.Context) {
	info, err := h.svc.InvitationInfo(c.Request.Context(), c.Param("token"))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, info)
}

func (h *Handler) AcceptInvitation(c *gin.Context) {
	var in AcceptInvitationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	session, err := h.svc.AcceptInvitation(c.Request.Context(), c.Param("token"), in, requestMeta(c))
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, session)
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
