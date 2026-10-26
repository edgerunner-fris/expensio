package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	headerAuthorization = "Authorization"
	bearerPrefix        = "bearer "
)

// AccessParser 是中间件对令牌校验器的最小依赖，便于在测试里替换。
type AccessParser interface {
	ParseAccess(raw string) (*auth.Claims, error)
}

// Auth 校验 access token 并把租户上下文写入 request context。
//
// 网关与各业务服务都挂这一份实现：网关负责尽早拒绝无效请求，
// 业务服务不信任网关转发的任何头，自己再解一次。
// 这样即使有人绕过网关直连服务端口，也拿不到未认证的数据。
func Auth(parser AccessParser) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := Authenticate(c, parser); err != nil {
			httpx.Error(c, err)
			return
		}
		c.Next()
	}
}

// Authenticate 是 Auth 的内核，供网关的按路由表转发复用：
// 网关不是"整站都要认证"——注册、登录、支付回调都是公开路径，
// 认证与否要由路由规则决定，所以这里提供可单独调用的版本。
func Authenticate(c *gin.Context, parser AccessParser) error {
	raw, ok := bearerToken(c.GetHeader(headerAuthorization))
	if !ok {
		return apperr.Unauthorized("缺少访问令牌")
	}

	claims, err := parser.ParseAccess(raw)
	if err != nil {
		// 具体原因（签名不符 / 过期 / 算法不对）只进日志，不进响应：
		// 对攻击者来说，"签名错"与"过期"是可利用的信息差。
		return apperr.Unauthorized("登录状态已失效，请重新登录").WithCause(err)
	}

	t, err := claims.Tenant()
	if err != nil {
		return apperr.Unauthorized("登录状态已失效，请重新登录").WithCause(err)
	}

	ctx, err := tenantctx.With(c.Request.Context(), t)
	if err != nil {
		return apperr.Internal(err)
	}
	c.Request = c.Request.WithContext(ctx)
	return nil
}

// RequireRole 要求当前角色不低于 min。必须挂在 Auth 之后。
func RequireRole(min tenantctx.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		t, ok := tenantctx.From(c.Request.Context())
		if !ok {
			httpx.Error(c, apperr.Unauthorized("缺少访问令牌"))
			return
		}
		if !t.Role.AtLeast(min) {
			httpx.Error(c, apperr.Forbidden("当前角色无权执行此操作"))
			return
		}
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	// 允许大小写不敏感（部分客户端会发 "bearer"），其余格式一律拒绝。
	if len(header) <= len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
