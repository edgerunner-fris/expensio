// Package server 负责 HTTP 服务器的装配与生命周期。
//
// 关闭顺序是硬性要求：
// SIGTERM → readiness 置 false → 排空窗口 → Shutdown（等在途请求）→ 退出。
// 反过来做（先 Shutdown 再摘流量）会在滚动更新时产生 5xx。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/version"
)

// Options 是启动一个服务所需的全部参数。
type Options struct {
	Addr            string
	Mode            string // debug / release / test
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	// DrainDelay 是 readiness 置 false 到真正关闭监听之间的等待，
	// 给 K8s 时间把本实例从 Endpoints 摘掉。本地运行设为 0。
	DrainDelay time.Duration
	Logger     *zap.Logger
	Health     *health.Registry
	// Middleware 在 RequestID 之前安装，通常用于 trace/metrics。
	// 放在最外层才能把 Recovery 写回的 500 计入指标。
	Middleware []gin.HandlerFunc
	// TrustedProxies 为 nil 时不信任任何代理头；非空时只采信来自这些
	// IP/CIDR 的 X-Forwarded-For。业务层依赖 ClientIP 做限流与审计。
	TrustedProxies []string
}

type Server struct {
	httpServer      *http.Server
	engine          *gin.Engine
	log             *zap.Logger
	shutdownTimeout time.Duration
	drainDelay      time.Duration
	ready           bool // 用 mutex 保护，读写都在低频路径上
	mu              sync.RWMutex
	listenAddr      string
}

func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("server: Addr 不能为空")
	}
	if opts.Logger == nil {
		return nil, errors.New("server: Logger 不能为空")
	}
	if opts.Health == nil {
		return nil, errors.New("server: Health 不能为空")
	}

	setGinMode(opts.Mode)

	engine := gin.New()
	// 默认不信任任何代理头：Gin 的默认行为会采信 X-Forwarded-For，
	// 任何人都能伪造 IP 写进审计日志。只有显式配置了可信代理网段才解析它。
	if err := engine.SetTrustedProxies(opts.TrustedProxies); err != nil {
		return nil, fmt.Errorf("server: 设置可信代理失败: %w", err)
	}
	for _, mw := range opts.Middleware {
		if mw != nil {
			engine.Use(mw)
		}
	}
	engine.Use(middleware.RequestID())
	engine.Use(middleware.AccessLog(opts.Logger))
	engine.Use(middleware.Recovery(opts.Logger))
	engine.NoRoute(func(c *gin.Context) {
		httpx.Error(c, apperr.NotFound("接口"))
	})

	s := &Server{
		engine:          engine,
		log:             opts.Logger,
		shutdownTimeout: opts.ShutdownTimeout,
		drainDelay:      opts.DrainDelay,
	}

	engine.GET("/healthz", opts.Health.Live)
	engine.GET("/readyz", s.readyHandler(opts.Health))

	readTimeout := opts.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = 10 * time.Second
	}
	writeTimeout := opts.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 15 * time.Second
	}
	idleTimeout := opts.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	if s.shutdownTimeout <= 0 {
		s.shutdownTimeout = 20 * time.Second
	}

	s.httpServer = &http.Server{
		Addr:              opts.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		// ErrorLog 交给 zap，避免标准库日志格式混进 JSON 日志流。
		ErrorLog: zap.NewStdLog(opts.Logger),
	}

	return s, nil
}

func setGinMode(mode string) {
	switch mode {
	case "debug":
		gin.SetMode(gin.DebugMode)
	case "test":
		gin.SetMode(gin.TestMode)
	default:
		gin.SetMode(gin.ReleaseMode)
	}
}

// Engine 暴露路由以便各服务注册自己的业务路由。
func (s *Server) Engine() *gin.Engine {
	return s.engine
}

// Addr 返回实际监听地址（端口传 :0 时用于测试）。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listenAddr
}

func (s *Server) setReady(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = v
}

func (s *Server) isReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func (s *Server) readyHandler(h *health.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.isReady() {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status":  "shutting_down",
				"version": version.Version,
			})
			return
		}
		h.Ready(c)
	}
}

// Run 阻塞直到 ctx 取消或服务异常退出。
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("server: 监听 %s 失败: %w", s.httpServer.Addr, err)
	}

	s.mu.Lock()
	s.listenAddr = ln.Addr().String()
	s.ready = true
	s.mu.Unlock()

	// version 已由 logger 附加为固定字段，这里只补本次监听的实际地址。
	s.log.Info("http server started",
		zap.String("addr", ln.Addr().String()),
	)

	serveErr := make(chan error, 1)
	go func() {
		err := s.httpServer.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// 1. 先摘流量
	s.setReady(false)
	// 关闭 keep-alive：本轮在途请求结束后连接直接断开，不再回到空闲池，
	// 避免 Shutdown 结束时还留着一批等下一次请求的连接。
	s.httpServer.SetKeepAlivesEnabled(false)
	s.log.Info("http server draining", zap.Duration("drain_delay", s.drainDelay))

	// 2. 排空窗口（不用 time.Sleep，便于被强杀时提前退出）
	if s.drainDelay > 0 {
		t := time.NewTimer(s.drainDelay)
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}

	// 3. 等在途请求结束。
	// 注意：net/http 会把"已建立 TCP 但还没发来请求头"的连接（StateNew）
	// 视作可能活跃，直到它建立满 5 秒（golang/go#22682）。Chrome 预连接、
	// 客户端连接池预拨都会产生这种连接，因此 ShutdownTimeout 必须大于 5s，
	// 否则滚动更新时会把正常关闭误判为超时。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		// 超时后强制断开剩余连接，不再等待。
		_ = s.httpServer.Close()
		return fmt.Errorf("server: 优雅关闭超时: %w", err)
	}

	s.log.Info("http server stopped")
	return nil
}
