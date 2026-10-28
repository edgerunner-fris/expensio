// Package gateway 是六个服务之前的唯一入口。
//
// 它只做四件事，按顺序：匹配路由 → 校验身份 → 限流 → 转发。
// 业务规则一律不在这里实现——网关里出现的任何"如果租户是 X 就……"，
// 都会在业务服务里再出现一次，然后两边慢慢分叉。
//
// 身份头处理是有意的：网关绝不把 X-Tenant-Id / X-User-Id 之类交给下游。
// 下游的租户上下文只能来自它自己校验过的 JWT，否则"网关漏配一条路由"
// 就会直接变成跨租户读取。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 限流决策单独计数：429 只能说明"拒绝过"，看不出 Redis 故障导致的 fail-open。
// 这里把 allowed / denied / error 三种结果都暴露出来，告警才能区分
// "正常限流" 与 "限流器已经失效、全站在裸奔"。
var rateLimitDecisions = telemetry.NewCounter(
	"expensio.gateway.rate_limit.decisions",
	"网关限流决策数（按范围与结果）",
	"{decision}",
)

// LimitScope 决定按什么维度限流。
type LimitScope int

const (
	// LimitByIP 用于未认证接口：还不知道租户是谁，只能按来源控制。
	LimitByIP LimitScope = iota
	// LimitByTenant 用于业务接口：租户 ID 来自 JWT，无法伪造。
	LimitByTenant
	// LimitNone 预留给健康检查之类的内部路径。
	LimitNone
)

// Rule 是一条前缀路由。匹配规则是"相等或以该前缀加斜杠开头"，
// 不做子串匹配：/api/v1/me 不该把 /api/v1/membership 一起吃掉。
type Rule struct {
	Prefix   string
	Upstream string
	// Auth 为 false 的只有两类：登录注册这类"换取凭证"的接口，
	// 和由下游用签名而非 JWT 验证的支付回调。
	Auth  bool
	Scope LimitScope
}

// DefaultRules 是 v1 的路由表。最长前缀优先由 New 负责排序。
func DefaultRules() []Rule {
	return []Rule{
		// 这三条挂在 /auth 下但要求已认证身份：不能落进下面"按 IP 限流、
		// 不校验令牌"的登录桶，否则正常使用会和登录尝试抢 20/min 的额度。
		{Prefix: "/api/v1/auth/tenants", Upstream: "identity", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/auth/switch-tenant", Upstream: "identity", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/auth/change-password", Upstream: "identity", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/auth", Upstream: "identity", Auth: false, Scope: LimitByIP},
		// 邀请接受页是公开的：令牌就是凭据，按 IP 限流防止令牌被暴力枚举。
		{Prefix: "/api/v1/invitations", Upstream: "identity", Auth: false, Scope: LimitByIP},
		{Prefix: "/api/v1/me", Upstream: "identity", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/tenants", Upstream: "identity", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/audit-logs", Upstream: "identity", Auth: true, Scope: LimitByTenant},

		// 预算归 expense 服务所有（ADR-0003）：预算预占与费用提交必须在同一个事务里，
		// 拆到独立服务就只能靠分布式事务来保证一致。
		{Prefix: "/api/v1/expenses", Upstream: "expense", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/budgets", Upstream: "expense", Auth: true, Scope: LimitByTenant},

		{Prefix: "/api/v1/invoices", Upstream: "billing", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/billing", Upstream: "billing", Auth: true, Scope: LimitByTenant},
		// 支付回调没有 JWT，由 billing 校验签名与幂等键。
		{Prefix: "/api/v1/webhooks", Upstream: "billing", Auth: false, Scope: LimitByIP},

		{Prefix: "/api/v1/reports", Upstream: "report", Auth: true, Scope: LimitByTenant},
		{Prefix: "/api/v1/notifications", Upstream: "notification", Auth: true, Scope: LimitByTenant},
	}
}

// Limits 是两类接口的令牌桶参数，单位是"每秒补充"与"桶容量"。
type Limits struct {
	AuthPerSecond float64
	AuthBurst     int
	APIPerSecond  float64
	APIBurst      int
}

// Options 是网关的装配参数。
type Options struct {
	Parser    middleware.AccessParser
	Upstreams map[string]string // 名字 → 完整 base URL
	Rules     []Rule
	Limiter   Limiter
	Limits    Limits
	Logger    *zap.Logger
	// MaxBodyBytes 只用于按 Content-Length 提前拒绝明显过大的请求；
	// 分块传输的实际上限由各业务服务自己判断，网关不读请求体。
	MaxBodyBytes int64
}

const defaultMaxBodyBytes = 25 << 20 // 25 MiB：够放收据图片，又不至于让单个请求吃满内存

type Handler struct {
	parser  middleware.AccessParser
	proxies map[string]*httputil.ReverseProxy
	rules   []Rule
	limiter Limiter
	limits  Limits
	log     *zap.Logger
	maxBody int64
}

// New 构造网关处理器。任何装配错误都在启动阶段暴露，不留到第一个请求。
func New(opts Options) (*Handler, error) {
	if opts.Parser == nil {
		return nil, errors.New("gateway: 缺少令牌解析器")
	}
	if opts.Logger == nil {
		return nil, errors.New("gateway: 缺少日志器")
	}
	if len(opts.Rules) == 0 {
		return nil, errors.New("gateway: 路由表为空")
	}

	h := &Handler{
		parser:  opts.Parser,
		proxies: make(map[string]*httputil.ReverseProxy, len(opts.Upstreams)),
		limiter: opts.Limiter,
		limits:  opts.Limits,
		log:     opts.Logger,
		maxBody: opts.MaxBodyBytes,
	}
	if h.maxBody <= 0 {
		h.maxBody = defaultMaxBodyBytes
	}

	seen := make(map[string]struct{}, len(opts.Rules))
	for _, r := range opts.Rules {
		if !strings.HasPrefix(r.Prefix, "/api/") {
			return nil, fmt.Errorf("gateway: 路由前缀必须位于 /api/ 下，当前为 %q", r.Prefix)
		}
		if _, dup := seen[r.Prefix]; dup {
			return nil, fmt.Errorf("gateway: 路由前缀重复: %s", r.Prefix)
		}
		seen[r.Prefix] = struct{}{}
		if _, ok := opts.Upstreams[r.Upstream]; !ok {
			return nil, fmt.Errorf("gateway: 路由 %s 指向未配置的下游 %q", r.Prefix, r.Upstream)
		}
	}
	h.rules = append(h.rules, opts.Rules...)
	// 最长前缀优先：将来若出现 /api/v1/tenants/:id 与 /api/v1/tenants/:id/members，
	// 更具体的必须先生效，而不是依赖注册顺序。
	sort.SliceStable(h.rules, func(i, j int) bool {
		return len(h.rules[i].Prefix) > len(h.rules[j].Prefix)
	})

	for name, raw := range opts.Upstreams {
		proxy, err := newUpstreamProxy(name, raw, opts.Logger)
		if err != nil {
			return nil, err
		}
		h.proxies[name] = proxy
	}
	return h, nil
}

// Handle 是挂在 /api/v1/*path 上的唯一处理入口。
func (h *Handler) Handle(c *gin.Context) {
	rule, ok := h.match(c.Request.URL.Path)
	if !ok {
		httpx.Error(c, apperr.NotFound("接口"))
		return
	}

	if rule.Auth {
		if err := middleware.Authenticate(c, h.parser); err != nil {
			httpx.Error(c, err)
			return
		}
	}

	if c.Request.ContentLength > h.maxBody {
		httpx.Error(c, apperr.New("common.payload_too_large", http.StatusRequestEntityTooLarge,
			"请求体过大"))
		return
	}

	if !h.allow(c, rule) {
		return
	}

	// 不信任客户端传来的任何身份头：下游只认自己解析的 JWT。
	// 即便下游今天不看这些头，留着也是在为将来某次"图省事"埋雷。
	for _, name := range []string{"X-Tenant-Id", "X-Tenant-Slug", "X-User-Id", "X-User-Role"} {
		c.Request.Header.Del(name)
	}
	c.Request.Header.Set("X-Request-ID", httpx.RequestID(c))

	h.proxies[rule.Upstream].ServeHTTP(c.Writer, c.Request)
}

// allow 执行限流。限流器故障时放行（fail-open）：
// 一个可用的系统偶尔超额，好过一个 Redis 抖动就让全站 5xx。
func (h *Handler) allow(c *gin.Context, rule Rule) bool {
	if h.limiter == nil || rule.Scope == LimitNone {
		return true
	}

	var (
		key      string
		perSec   float64
		burst    int
		scopeTag string
	)
	switch rule.Scope {
	case LimitByTenant:
		t, ok := tenantctx.From(c.Request.Context())
		if !ok {
			httpx.Error(c, apperr.Internal(errors.New("gateway: 已认证请求缺少租户上下文")))
			return false
		}
		key, scopeTag = "expensio:rl:tenant:"+t.ID.String(), "tenant"
		perSec, burst = h.limits.APIPerSecond, h.limits.APIBurst
	default:
		key, scopeTag = "expensio:rl:ip:"+c.ClientIP(), "ip"
		perSec, burst = h.limits.AuthPerSecond, h.limits.AuthBurst
	}

	ctx := c.Request.Context()
	decision, err := h.limiter.Allow(ctx, key, perSec, burst)
	if err != nil {
		rateLimitDecisions.Add(ctx, 1,
			attribute.String("scope", scopeTag),
			attribute.String("outcome", "error"),
		)
		h.log.Warn("gateway: 限流器不可用，本次放行",
			zap.String("scope", scopeTag),
			zap.String("path", c.Request.URL.Path),
			zap.Error(err),
		)
		return true
	}
	if decision.Allowed {
		rateLimitDecisions.Add(ctx, 1,
			attribute.String("scope", scopeTag),
			attribute.String("outcome", "allowed"),
		)
		return true
	}

	rateLimitDecisions.Add(ctx, 1,
		attribute.String("scope", scopeTag),
		attribute.String("outcome", "denied"),
	)
	c.Header("Retry-After", retryAfterHeader(decision.RetryAfter))
	httpx.Error(c, apperr.New("common.rate_limited", http.StatusTooManyRequests,
		"请求过于频繁，请稍后再试").WithDetail("scope", scopeTag))
	return false
}

func retryAfterHeader(d time.Duration) string {
	secs := int(d / time.Second)
	if d%time.Second > 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%d", secs)
}

func (h *Handler) match(path string) (Rule, bool) {
	for _, r := range h.rules {
		if path == r.Prefix || strings.HasPrefix(path, r.Prefix+"/") {
			return r, true
		}
	}
	return Rule{}, false
}

// newUpstreamProxy 构造指向单个下游的反向代理。
//
// 超时策略：连接 2s、等响应头 10s。整体时长由 http.Server 的 WriteTimeout 兜底
// （当前 10s，报表出现慢查询时按需上调，而不是在网关里再压一层）。
func newUpstreamProxy(name, raw string, logg *zap.Logger) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("gateway: 解析下游 %s 地址失败: %w", name, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("gateway: 下游 %s 地址不完整: %q", name, raw)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}

	return &httputil.ReverseProxy{
		// Rewrite（而不是已弃用的 Director）让 ReverseProxy 先清掉入站的
		// X-Forwarded-*，再按本连接的真实来源重写，客户端无法伪造。
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		// 包在重试之外：每次实际下发（含重试）都有独立的 client span，
		// 并把 traceparent 注入到下游请求头，业务服务才能续接同一条链路。
		Transport:     &retryTransport{base: otelhttp.NewTransport(transport)},
		FlushInterval: 100 * time.Millisecond,
		// ReverseProxy 自带的日志会绕过 zap 的输出格式，直接丢弃，
		// 由下面的 ErrorHandler 统一记录。
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logg.Warn("gateway: 下游请求失败",
				zap.String("upstream", name),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Error(err),
			)
			writeUpstreamError(w, r)
		},
	}, nil
}

// writeUpstreamError 直接写 JSON，不经过 gin：
// ReverseProxy 在自己的 goroutine 里调用它，此时 gin 的上下文已不适合再使用。
func writeUpstreamError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(httpx.Envelope{
		Code:      "common.upstream_unavailable",
		Message:   "上游服务暂不可用，请稍后重试",
		RequestID: r.Header.Get("X-Request-ID"),
	})
}
