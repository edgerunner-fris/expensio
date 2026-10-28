package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

const (
	testTenant = "11111111-1111-1111-1111-111111111111"
	testUser   = "22222222-2222-2222-2222-222222222222"
)

// fakeParser 让测试不依赖真实签名：网关只关心 claims 的内容。
type fakeParser struct{}

func (fakeParser) ParseAccess(raw string) (*auth.Claims, error) {
	if raw != "good" {
		return nil, errors.New("签名不匹配")
	}
	return &auth.Claims{TenantID: testTenant, Role: "owner", Subject: testUser}, nil
}

type fakeLimiter struct {
	decision Decision
	err      error
	keys     []string
}

func (l *fakeLimiter) Allow(_ context.Context, key string, perSecond float64, burst int) (Decision, error) {
	l.keys = append(l.keys, key)
	return l.decision, l.err
}

// upstream 是一个记录请求的下游服务，返回的 JSON 里带自己的名字。
type upstream struct {
	*httptest.Server
	hits    int
	last    http.Header
	lastURL string
}

func newUpstream(t *testing.T, name string) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits++
		u.last = r.Header.Clone()
		u.lastURL = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":"ok","service":"`+name+`"}`)
	}))
	t.Cleanup(u.Close)
	return u
}

// startGateway 用真实 HTTP 服务器承载 gin 引擎。
// 不用 httptest.NewRecorder：gin 的 responseWriter 依赖底层 writer 支持 CloseNotify，
// 只有真实的 http.response 才实现它，而 ReverseProxy 会用到。
func startGateway(t *testing.T, upstreams map[string]string, rules []Rule, limiter Limiter) *httptest.Server {
	t.Helper()
	// DefaultRules 引用了全部五个下游，缺一个 New 就会拒绝装配。
	// 测试只关心其中一两个，其余指向同一个假下游即可。
	full := make(map[string]string, len(upstreams))
	for name, url := range upstreams {
		full[name] = url
	}
	fallback := upstreams["identity"]
	require.NotEmpty(t, fallback, "测试至少要提供 identity 下游")
	for _, name := range []string{"identity", "expense", "billing", "report", "notification"} {
		if _, ok := full[name]; !ok {
			full[name] = fallback
		}
	}

	h, err := New(Options{
		Parser:    fakeParser{},
		Upstreams: full,
		Rules:     rules,
		Limiter:   limiter,
		Limits:    Limits{AuthPerSecond: 1, AuthBurst: 10, APIPerSecond: 10, APIBurst: 20},
		Logger:    zap.NewNop(),
	})
	require.NoError(t, err)

	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Any("/api/v1/*path", h.Handle)

	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv
}

type response struct {
	status int
	header http.Header
	body   string
}

func call(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string) response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func envelopeOf(t *testing.T, body string) httpx.Envelope {
	t.Helper()
	var env httpx.Envelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	return env
}

func TestGatewayRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	identity := newUpstream(t, "identity")
	expense := newUpstream(t, "expense")
	billingSvc := newUpstream(t, "billing")
	reportSvc := newUpstream(t, "report")
	notificationSvc := newUpstream(t, "notification")
	upstreams := map[string]string{
		"identity":     identity.URL,
		"expense":      expense.URL,
		"billing":      billingSvc.URL,
		"report":       reportSvc.URL,
		"notification": notificationSvc.URL,
	}

	t.Run("公开路由不要求令牌", func(t *testing.T) {
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		got := call(t, srv, http.MethodPost, "/api/v1/auth/login", nil)

		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, 1, identity.hits)
		assert.Contains(t, got.body, "identity")
	})

	t.Run("受保护路由无令牌直接拒绝且不触达下游", func(t *testing.T) {
		before := identity.hits
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		got := call(t, srv, http.MethodGet, "/api/v1/me", nil)

		assert.Equal(t, http.StatusUnauthorized, got.status)
		assert.Equal(t, "auth.unauthorized", envelopeOf(t, got.body).Code)
		assert.Equal(t, before, identity.hits, "未认证请求不能到达下游")
	})

	t.Run("有效令牌被转发且剥掉伪造的身份头", func(t *testing.T) {
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		got := call(t, srv, http.MethodGet, "/api/v1/me?verbose=1", map[string]string{
			"Authorization": "Bearer good",
			"X-Tenant-Id":   "99999999-9999-9999-9999-999999999999",
			"X-User-Id":     "88888888-8888-8888-8888-888888888888",
			"X-User-Role":   "owner",
		})

		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, "/api/v1/me?verbose=1", identity.lastURL, "路径与查询串必须原样转发")

		// 客户端伪造的身份头必须被剥掉：下游只能信自己解析的 JWT。
		assert.Empty(t, identity.last.Get("X-Tenant-Id"))
		assert.Empty(t, identity.last.Get("X-User-Id"))
		assert.Empty(t, identity.last.Get("X-User-Role"))
		assert.Equal(t, "Bearer good", identity.last.Get("Authorization"))
		assert.NotEmpty(t, identity.last.Get("X-Request-ID"))
	})

	t.Run("报表路由转发到 report 且保留路径与查询串", func(t *testing.T) {
		before := identity.hits
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		const path = "/api/v1/reports/expenses/summary?from=2026-10-01&to=2026-10-31&scope=all"
		got := call(t, srv, http.MethodGet, path, map[string]string{
			"Authorization": "Bearer good",
		})

		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, path, reportSvc.lastURL)
		assert.Contains(t, got.body, "report")
		assert.Equal(t, before, identity.hits, "报表请求不能落到 identity")
	})

	t.Run("通知路由要求令牌且只转发给 notification", func(t *testing.T) {
		beforeIdentity := identity.hits
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		const path = "/api/v1/notifications?unread_only=true&page=1&page_size=20"

		unauthenticated := call(t, srv, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusUnauthorized, unauthenticated.status)
		assert.Equal(t, beforeIdentity, identity.hits, "未认证请求不能到达任何下游")

		got := call(t, srv, http.MethodGet, path, map[string]string{"Authorization": "Bearer good"})
		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, path, notificationSvc.lastURL)
		assert.Contains(t, got.body, "notification")
		assert.Equal(t, beforeIdentity, identity.hits, "通知请求不能落到 identity")
	})

	t.Run("支付回调不要求令牌且只转发给 billing", func(t *testing.T) {
		beforeIdentity := identity.hits
		beforeBilling := billingSvc.hits
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		const path = "/api/v1/webhooks/payments"
		got := call(t, srv, http.MethodPost, path, nil)

		assert.Equal(t, http.StatusOK, got.status)
		assert.Equal(t, beforeBilling+1, billingSvc.hits)
		assert.Equal(t, path, billingSvc.lastURL)
		assert.Equal(t, beforeIdentity, identity.hits, "回调不能落到 identity")
	})

	t.Run("未知路径返回统一信封", func(t *testing.T) {
		srv := startGateway(t, upstreams, DefaultRules(), nil)
		got := call(t, srv, http.MethodGet, "/api/v1/unknown", nil)

		assert.Equal(t, http.StatusNotFound, got.status)
		assert.Equal(t, "common.not_found", envelopeOf(t, got.body).Code)
	})

	t.Run("最长前缀优先", func(t *testing.T) {
		before := identity.hits
		srv := startGateway(t, upstreams, []Rule{
			{Prefix: "/api/v1/tenants", Upstream: "identity", Auth: true},
			{Prefix: "/api/v1/tenants/exports", Upstream: "expense", Auth: true},
		}, nil)
		got := call(t, srv, http.MethodGet, "/api/v1/tenants/exports/1", map[string]string{"Authorization": "Bearer good"})

		assert.Equal(t, http.StatusOK, got.status)
		assert.Contains(t, got.body, "expense")
		assert.Equal(t, before, identity.hits)
	})
}

func TestGatewayRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	identity := newUpstream(t, "identity")
	upstreams := map[string]string{"identity": identity.URL}

	t.Run("限流命中返回 429 与 Retry-After", func(t *testing.T) {
		lim := &fakeLimiter{decision: Decision{Allowed: false, RetryAfter: 1500 * time.Millisecond}}
		srv := startGateway(t, upstreams, DefaultRules(), lim)
		got := call(t, srv, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer good"})

		assert.Equal(t, http.StatusTooManyRequests, got.status)
		assert.Equal(t, "2", got.header.Get("Retry-After"))
		assert.Equal(t, "common.rate_limited", envelopeOf(t, got.body).Code)
	})

	t.Run("业务接口按租户限流，认证接口按 IP 限流", func(t *testing.T) {
		lim := &fakeLimiter{decision: Decision{Allowed: true}}
		srv := startGateway(t, upstreams, DefaultRules(), lim)

		call(t, srv, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer good"})
		call(t, srv, http.MethodPost, "/api/v1/auth/login", nil)

		require.Len(t, lim.keys, 2)
		assert.Equal(t, "expensio:rl:tenant:"+testTenant, lim.keys[0])
		assert.True(t, strings.HasPrefix(lim.keys[1], "expensio:rl:ip:"), lim.keys[1])
	})

	t.Run("限流器故障时放行", func(t *testing.T) {
		lim := &fakeLimiter{err: errors.New("redis down")}
		srv := startGateway(t, upstreams, DefaultRules(), lim)
		got := call(t, srv, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer good"})

		assert.Equal(t, http.StatusOK, got.status, "限流器不可用不应拖垮全站")
	})
}

func TestGatewayUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 端口已关闭：连接必然被拒

	srv := startGateway(t, map[string]string{"identity": deadURL},
		[]Rule{{Prefix: "/api/v1/me", Upstream: "identity", Auth: true}}, nil)
	got := call(t, srv, http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer good"})

	assert.Equal(t, http.StatusBadGateway, got.status)
	assert.Equal(t, "common.upstream_unavailable", envelopeOf(t, got.body).Code)
}

func TestNewRejectsMisconfiguration(t *testing.T) {
	upstreams := map[string]string{"identity": "http://127.0.0.1:1"}

	cases := map[string]Options{
		"前缀不在 /api 下": {Parser: fakeParser{}, Upstreams: upstreams,
			Rules: []Rule{{Prefix: "/health", Upstream: "identity"}}, Logger: zap.NewNop()},
		"路由指向未配置的下游": {Parser: fakeParser{}, Upstreams: upstreams,
			Rules: []Rule{{Prefix: "/api/v1/me", Upstream: "nope"}}, Logger: zap.NewNop()},
		"下游地址非法": {Parser: fakeParser{}, Upstreams: map[string]string{"identity": "not-a-url"},
			Rules: []Rule{{Prefix: "/api/v1/me", Upstream: "identity"}}, Logger: zap.NewNop()},
		"缺少解析器": {Upstreams: upstreams,
			Rules: []Rule{{Prefix: "/api/v1/me", Upstream: "identity"}}, Logger: zap.NewNop()},
		"路由前缀重复": {Parser: fakeParser{}, Upstreams: upstreams,
			Rules: []Rule{
				{Prefix: "/api/v1/me", Upstream: "identity"},
				{Prefix: "/api/v1/me", Upstream: "identity"},
			}, Logger: zap.NewNop()},
		"熔断参数为负": {Parser: fakeParser{}, Upstreams: upstreams,
			Rules:   []Rule{{Prefix: "/api/v1/me", Upstream: "identity"}},
			Circuit: CircuitOptions{Failures: -1}, Logger: zap.NewNop()},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(opts)
			assert.Error(t, err)
		})
	}
}

func TestRetryOnlyWhenRequestCannotHaveArrived(t *testing.T) {
	get := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.True(t, retryable(get))

	post := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	assert.False(t, retryable(post))

	// GET 带 body 时同样不重试：body 读过一次就不保证能原样重放。
	getWithBody := httptest.NewRequest(http.MethodGet, "/", strings.NewReader("x"))
	assert.False(t, retryable(getWithBody))

	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	assert.True(t, connectionFailed(refused))

	assert.False(t, connectionFailed(context.Canceled), "超时不能重试：请求可能已经送达")
	assert.False(t, connectionFailed(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")}))
}
