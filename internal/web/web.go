// Package web 提供管理台的静态资源服务与到 gateway 的反向代理。
//
// 之所以不用 nginx：构建环境里 nginx 镜像无法稳定拉取，而这点功能用
// 标准库就能完成，还能顺便复用 Go 的优雅关闭与结构化日志。它的职责只有三件：
//
//  1. 提供 Vite 构建产物；找不到的路径回退到 index.html，交给 Vue Router；
//  2. 把 /api/ 原样转发给 gateway（浏览器只与本服务同源，不需要 CORS）；
//  3. /healthz 与 /readyz 供容器与 K8s 探针使用。
//
// 本服务不解析、不签发、不校验令牌，也不写数据库：认证仍然由网关和
// 下游服务完成，这里只是传输层。
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
)

const (
	apiPrefix    = "/api/"
	indexFile    = "index.html"
	assetsPrefix = "assets/"
)

// Options 是构造 Server 所需的全部输入。
type Options struct {
	// Root 是 Vite 的 dist 目录，必须存在且包含 index.html。
	Root string
	// GatewayURL 形如 http://gateway:8080，不能带查询串或片段。
	GatewayURL string
	Logger     *zap.Logger
	Version    string
}

// Server 组合了静态文件、反向代理与探针处理器。
type Server struct {
	root      string
	index     string
	version   string
	logger    *zap.Logger
	handler   http.Handler
	proxy     *httputil.ReverseProxy
	gateway   *url.URL
	transport *http.Transport
}

// New 校验配置并构造处理器。任何错误都在启动期返回，避免带着坏配置上线。
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("web: 缺少 logger")
	}
	if strings.TrimSpace(opts.Root) == "" {
		return nil, errors.New("web: 缺少静态资源目录 EXPENSIO_WEB_ROOT")
	}

	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("web: 解析静态资源目录失败: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("web: 静态资源目录不可用: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("web: 静态资源路径 %s 不是目录", root)
	}
	index := filepath.Join(root, indexFile)
	if _, err := os.Stat(index); err != nil {
		return nil, fmt.Errorf("web: 缺少 %s（是否忘记 npm run build？）: %w", indexFile, err)
	}

	gateway, err := parseGatewayURL(opts.GatewayURL)
	if err != nil {
		return nil, err
	}

	s := &Server{
		root:    root,
		index:   index,
		version: opts.Version,
		logger:  opts.Logger,
		gateway: gateway,
		transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}

	proxy := httputil.NewSingleHostReverseProxy(gateway)
	// 与网关一致：web 自己产生 client span，并把 traceparent 继续传给网关。
	proxy.Transport = otelhttp.NewTransport(s.transport)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// 只记录方法与路径，不记录请求体与 Authorization。
		s.logger.Error("gateway proxy failed",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Error(err))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"bad_gateway"}`))
	}
	s.proxy = proxy

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleHealth)
	mux.Handle(apiPrefix, proxy)
	mux.Handle("/", http.HandlerFunc(s.serveStatic))
	s.handler = securityHeaders(mux)
	return s, nil
}

// Handler 返回可以直接交给 http.Server 的处理器。
func (s *Server) Handler() http.Handler { return s.handler }

// RouteLabel 为 net/http 链路提供低基数路由标签。
// 静态资源返回空串，交给 telemetry.HTTPHandler 跳过：
// 一个页面会带出几十个 hashed 资源，逐个建 span 只会淹没真正的 API 链路。
func RouteLabel(r *http.Request) string {
	switch {
	case strings.HasPrefix(r.URL.Path, apiPrefix):
		return "api"
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		return ""
	case r.URL.Path == "/":
		return "index"
	default:
		return "spa"
	}
}

// Close 释放代理持有的空闲连接；停机时调用。
func (s *Server) Close() { s.transport.CloseIdleConnections() }

func parseGatewayURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("web: 缺少 EXPENSIO_GATEWAY_URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("web: 解析 EXPENSIO_GATEWAY_URL 失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("web: EXPENSIO_GATEWAY_URL 只支持 http/https，得到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("web: EXPENSIO_GATEWAY_URL 缺少主机名")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("web: EXPENSIO_GATEWAY_URL 不能带查询串或片段")
	}
	return u, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	body := struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Version string `json:"version"`
	}{Status: "ok", Service: "web", Version: s.version}
	_ = json.NewEncoder(w).Encode(body)
}

// serveStatic 只接受 GET/HEAD。存在的文件按扩展名决定缓存策略，
// 不存在的路径回退到 index.html；但静态资源目录与带扩展名的路径必须 404，
// 否则缺失的 JS 会拿到 HTML，浏览器报的是 MIME 错误而不是 404。
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clean := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(clean, "/")
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			// 不暴露 .env、.well-known 之外的隐藏文件；点开头的段一律 404。
			notFound(w)
			return
		}
	}

	if rel != "" {
		full := filepath.Join(s.root, filepath.FromSlash(rel))
		if !within(s.root, full) {
			notFound(w)
			return
		}
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			s.writeFile(w, r, full, rel)
			return
		}
	}

	if rel == "" || (!strings.Contains(rel, ".") && !strings.HasPrefix(rel, assetsPrefix)) {
		s.writeFile(w, r, s.index, indexFile)
		return
	}
	notFound(w)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request, full, rel string) {
	f, err := os.Open(full)
	if err != nil {
		notFound(w)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		notFound(w)
		return
	}

	// Vite 的 assets 文件名带内容哈希，可以长期缓存；其余（尤其是 index.html）
	// 必须每次回源，否则发版后用户会一直拿到旧的入口文件。
	if strings.HasPrefix(rel, assetsPrefix) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func within(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "404 page not found", http.StatusNotFound)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// Element Plus 会在运行时插入 <style>，所以 style-src 必须允许 inline；
		// 脚本仍然只允许同源文件，生产构建不需要 eval。
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
