package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

// switchableUpstream 的状态可以在测试中途切换，用来验证"熔断打开后恢复"。
// hits 用原子操作：服务端 goroutine 与测试 goroutine 之间没有显式同步，
// 普通 int 在 -race 下会被判为数据竞争。
type switchableUpstream struct {
	*httptest.Server
	hits   atomic.Int64
	status atomic.Int32
}

func newSwitchableUpstream(t *testing.T, status int) *switchableUpstream {
	t.Helper()
	u := &switchableUpstream{}
	u.status.Store(int32(status))
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		w.WriteHeader(int(u.status.Load()))
		_, _ = io.WriteString(w, "downstream")
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *switchableUpstream) setStatus(status int) { u.status.Store(int32(status)) }

// startCircuitGateway 只注册被测路由，避免 DefaultRules 要求五个下游全部配置。
func startCircuitGateway(t *testing.T, upstreams map[string]string, rules []Rule, circuit CircuitOptions) *httptest.Server {
	t.Helper()
	h, err := New(Options{
		Parser:    fakeParser{},
		Upstreams: upstreams,
		Rules:     rules,
		Circuit:   circuit,
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

var authHeader = map[string]string{"Authorization": "Bearer good"}

func meRule() []Rule {
	return []Rule{{Prefix: "/api/v1/me", Upstream: "identity", Auth: true, Scope: LimitNone}}
}

func TestCircuitBreakerOpensOnRepeated5xx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := newSwitchableUpstream(t, http.StatusServiceUnavailable)
	srv := startCircuitGateway(t,
		map[string]string{"identity": up.URL}, meRule(),
		CircuitOptions{Failures: 3, Timeout: time.Minute})

	// 阈值之前的失败请求仍然原样转发：熔断不是"第一次失败就拒绝"。
	for i := 0; i < 3; i++ {
		got := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
		require.Equal(t, http.StatusServiceUnavailable, got.status)
		require.Equal(t, "downstream", got.body, "下游响应必须原样返回")
	}
	require.EqualValues(t, 3, up.hits.Load())

	got := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	assert.Equal(t, http.StatusServiceUnavailable, got.status)
	assert.Equal(t, "common.upstream_circuit_open", envelopeOf(t, got.body).Code)
	assert.NotEmpty(t, got.header.Get("Retry-After"), "熔断期间应告知客户端何时重试")
	assert.EqualValues(t, 3, up.hits.Load(), "熔断打开后请求不能再触达下游")
}

func TestCircuitBreakerRecoversAfterTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := newSwitchableUpstream(t, http.StatusBadGateway)
	srv := startCircuitGateway(t,
		map[string]string{"identity": up.URL}, meRule(),
		CircuitOptions{Failures: 2, Timeout: 80 * time.Millisecond})

	for i := 0; i < 2; i++ {
		call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	}
	open := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	require.Equal(t, "common.upstream_circuit_open", envelopeOf(t, open.body).Code)

	// 下游恢复后，要等 open 超时到点才会放半开探测请求过去。
	up.setStatus(http.StatusOK)
	time.Sleep(150 * time.Millisecond)

	probe := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	require.Equal(t, http.StatusOK, probe.status, "半开探测成功应恢复转发")

	after := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	assert.Equal(t, http.StatusOK, after.status, "探测成功后熔断器应回到 closed")
	assert.EqualValues(t, 4, up.hits.Load(), "探测请求必须真的到达下游")
}

func TestCircuitBreakerIgnoresApplication500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := newSwitchableUpstream(t, http.StatusInternalServerError)
	srv := startCircuitGateway(t,
		map[string]string{"identity": up.URL}, meRule(),
		CircuitOptions{Failures: 2, Timeout: time.Minute})

	// 500 往往由个别坏请求触发；用它熔断会把一次坏输入放大成整个下游不可用。
	for i := 0; i < 5; i++ {
		got := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
		require.Equal(t, http.StatusInternalServerError, got.status)
	}
	assert.EqualValues(t, 5, up.hits.Load(), "500 不应触发熔断")
}

func TestCircuitBreakerIsolatedPerUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dying := newSwitchableUpstream(t, http.StatusServiceUnavailable)
	healthy := newSwitchableUpstream(t, http.StatusOK)
	srv := startCircuitGateway(t,
		map[string]string{"identity": dying.URL, "expense": healthy.URL},
		[]Rule{
			{Prefix: "/api/v1/me", Upstream: "identity", Auth: true, Scope: LimitNone},
			{Prefix: "/api/v1/expenses", Upstream: "expense", Auth: true, Scope: LimitNone},
		},
		CircuitOptions{Failures: 1, Timeout: time.Minute})

	first := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	require.Equal(t, http.StatusServiceUnavailable, first.status)
	open := call(t, srv, http.MethodGet, "/api/v1/me", authHeader)
	require.Equal(t, "common.upstream_circuit_open", envelopeOf(t, open.body).Code)

	got := call(t, srv, http.MethodGet, "/api/v1/expenses", authHeader)
	assert.Equal(t, http.StatusOK, got.status, "一个下游熔断不能影响其他下游")
	assert.EqualValues(t, 1, healthy.hits.Load())
}

// failingRoundTripper 让测试绕开真实网络，直接验证熔断器对错误的分类。
type failingRoundTripper struct {
	err error
}

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func TestCircuitTransportExcludesClientCancellation(t *testing.T) {
	breaker, err := newCircuitTransport("identity", failingRoundTripper{err: context.Canceled},
		CircuitOptions{Failures: 1, Timeout: time.Minute}, zap.NewNop())
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for i := 0; i < 3; i++ {
		_, roundErr := breaker.RoundTrip(req)
		require.ErrorIs(t, roundErr, context.Canceled)
	}
	assert.Equal(t, "closed", breaker.breaker.State().String(),
		"客户端取消是调用方走了，不是下游故障，不能熔断")
}

func TestCircuitTransportOpensOnTransportError(t *testing.T) {
	breaker, err := newCircuitTransport("identity", failingRoundTripper{err: errors.New("boom")},
		CircuitOptions{Failures: 2, Timeout: time.Minute}, zap.NewNop())
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for i := 0; i < 2; i++ {
		_, roundErr := breaker.RoundTrip(req)
		require.Error(t, roundErr)
		require.NotErrorIs(t, roundErr, errCircuitOpen)
	}

	_, roundErr := breaker.RoundTrip(req)
	assert.ErrorIs(t, roundErr, errCircuitOpen, "连接失败达到阈值后应主动拒绝")
}
