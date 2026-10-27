package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/web"
)

const testIndex = `<!doctype html><html><head><title>Expensio</title></head><body><div id="app"></div></body></html>`

func newDist(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.html"), []byte(testIndex), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "assets", "index-abc123.js"), []byte("console.log(1)\n"), 0o644))
	return root
}

func newServer(t *testing.T, root, gatewayURL string) *web.Server {
	t.Helper()
	srv, err := web.New(web.Options{
		Root:       root,
		GatewayURL: gatewayURL,
		Logger:     zap.NewNop(),
		Version:    "test",
	})
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewRejectsBadOptions(t *testing.T) {
	root := newDist(t)

	t.Run("缺少 logger", func(t *testing.T) {
		_, err := web.New(web.Options{Root: root, GatewayURL: "http://127.0.0.1:8080"})
		require.Error(t, err)
	})

	t.Run("目录不存在", func(t *testing.T) {
		_, err := web.New(web.Options{Root: filepath.Join(root, "missing"), GatewayURL: "http://127.0.0.1:8080", Logger: zap.NewNop()})
		require.Error(t, err)
	})

	t.Run("缺少 index.html", func(t *testing.T) {
		empty := t.TempDir()
		_, err := web.New(web.Options{Root: empty, GatewayURL: "http://127.0.0.1:8080", Logger: zap.NewNop()})
		require.Error(t, err)
	})

	for name, gateway := range map[string]string{
		"空地址":      "",
		"非法协议":     "ftp://127.0.0.1:8080",
		"缺少主机":     "http://",
		"带查询串":     "http://127.0.0.1:8080/?x=1",
		"无法解析的://": "http://[::1",
	} {
		t.Run("网关地址"+name, func(t *testing.T) {
			_, err := web.New(web.Options{Root: root, GatewayURL: gateway, Logger: zap.NewNop()})
			require.Error(t, err)
		})
	}
}

func TestStaticFilesAndSPAFallback(t *testing.T) {
	root := newDist(t)
	srv := newServer(t, root, "http://127.0.0.1:1")

	t.Run("根路径返回 index", func(t *testing.T) {
		rec := do(t, srv.Handler(), http.MethodGet, "/", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, testIndex, rec.Body.String())
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
		assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	})

	t.Run("深层前端路由回退 index", func(t *testing.T) {
		rec := do(t, srv.Handler(), http.MethodGet, "/expenses/42?tab=detail", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, testIndex, rec.Body.String())
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	})

	t.Run("带哈希的资源长期缓存", func(t *testing.T) {
		rec := do(t, srv.Handler(), http.MethodGet, "/assets/index-abc123.js", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "console.log(1)\n", rec.Body.String())
		assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	})

	t.Run("HEAD 不返回正文", func(t *testing.T) {
		rec := do(t, srv.Handler(), http.MethodHead, "/", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Empty(t, rec.Body.String())
	})

	t.Run("缺失资源返回 404 而不是 index", func(t *testing.T) {
		for _, target := range []string{"/assets/missing-xyz.js", "/favicon.ico"} {
			rec := do(t, srv.Handler(), http.MethodGet, target, nil, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, target)
			assert.NotContains(t, rec.Body.String(), "<title>Expensio</title>")
		}
	})

	t.Run("隐藏文件不可见", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600))
		rec := do(t, srv.Handler(), http.MethodGet, "/.env", nil, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("静态资源只允许读", func(t *testing.T) {
		rec := do(t, srv.Handler(), http.MethodPost, "/", strings.NewReader("x"), nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
	})
}

func TestHealth(t *testing.T) {
	srv := newServer(t, newDist(t), "http://127.0.0.1:1")
	for _, target := range []string{"/healthz", "/readyz"} {
		rec := do(t, srv.Handler(), http.MethodGet, target, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code, target)
		assert.JSONEq(t, `{"status":"ok","service":"web","version":"test"}`, rec.Body.String())
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestProxyForwardsToGateway(t *testing.T) {
	type captured struct {
		method string
		path   string
		query  string
		body   string
		auth   string
	}
	got := make(chan captured, 1)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body), auth: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "gateway")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"access_token":"t"}`))
	}))
	defer gateway.Close()

	srv := newServer(t, newDist(t), gateway.URL)

	rec := do(t, srv.Handler(), http.MethodPost, "/api/v1/auth/login?tenant=acme",
		strings.NewReader(`{"email":"a@b.c"}`), map[string]string{"Authorization": "Bearer abc"})
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"access_token":"t"}`, rec.Body.String())
	assert.Equal(t, "gateway", rec.Header().Get("X-Upstream"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	c := <-got
	assert.Equal(t, http.MethodPost, c.method)
	assert.Equal(t, "/api/v1/auth/login", c.path)
	assert.Equal(t, "tenant=acme", c.query)
	assert.JSONEq(t, `{"email":"a@b.c"}`, c.body)
	assert.Equal(t, "Bearer abc", c.auth)
}

func TestProxyReturnsBadGatewayWhenGatewayIsDown(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gatewayURL := gateway.URL
	gateway.Close()

	srv := newServer(t, newDist(t), gatewayURL)
	rec := do(t, srv.Handler(), http.MethodGet, "/api/v1/expenses", nil, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	assert.JSONEq(t, `{"error":"bad_gateway"}`, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}
