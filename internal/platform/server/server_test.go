package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/server"
)

func newServer(t *testing.T) *server.Server {
	t.Helper()

	reg := health.New("test")
	reg.Add(health.Check("db", func(context.Context) error { return nil }))

	s, err := server.New(server.Options{
		Addr:            "127.0.0.1:0",
		Mode:            "test",
		ShutdownTimeout: 2 * time.Second,
		Logger:          zap.NewNop(),
		Health:          reg,
	})
	require.NoError(t, err)

	s.Engine().GET("/ping", func(c *gin.Context) {
		httpx.OK(c, gin.H{"pong": true})
	})
	return s
}

func waitForAddr(t *testing.T, s *server.Server) string {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.Addr(); addr != "" {
			return addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("服务器未在 3 秒内开始监听")
	return ""
}

func TestServesProbesAndBusinessRoutes(t *testing.T) {
	s := newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	addr := waitForAddr(t, s)
	base := "http://" + addr

	// 专用 Transport 且禁用 keep-alive：默认 Transport 的连接池会为可能的
	// 重试预拨连接，这类连接不发请求、停在 StateNew，net/http 要等满 5 秒
	// 才认作空闲（golang/go#22682），会让关闭耗时断言随调度抖动。
	// 这里要验证的是服务端关闭语义，不应该被客户端的连接池行为干扰。
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   3 * time.Second,
	}
	defer client.CloseIdleConnections()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusOK},
		{"/ping", http.StatusOK},
		{"/nope", http.StatusNotFound},
	} {
		resp, err := client.Get(base + tc.path)
		require.NoError(t, err, tc.path)
		resp.Body.Close()
		assert.Equal(t, tc.want, resp.StatusCode, tc.path)
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("关闭超时")
	}
}

func TestTrustedProxiesControlClientIP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted []string
		want    string
	}{
		{name: "默认不信任 X-Forwarded-For", want: "10.1.2.3"},
		{name: "可信代理网段内采信 X-Forwarded-For", trusted: []string{"10.0.0.0/8"}, want: "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := server.New(server.Options{
				Addr:           "127.0.0.1:0",
				Mode:           "test",
				Logger:         zap.NewNop(),
				Health:         health.New("test"),
				TrustedProxies: tc.trusted,
			})
			require.NoError(t, err)
			s.Engine().GET("/ip", func(c *gin.Context) {
				c.String(http.StatusOK, c.ClientIP())
			})

			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = "10.1.2.3:1234"
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			rec := httptest.NewRecorder()
			s.Engine().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, rec.Body.String())
		})
	}
}

func TestRunReturnsErrorWhenPortInvalid(t *testing.T) {
	reg := health.New("test")
	s, err := server.New(server.Options{
		Addr:   "127.0.0.1:70000",
		Mode:   "test",
		Logger: zap.NewNop(),
		Health: reg,
	})
	require.NoError(t, err)

	err = s.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "监听")
}
