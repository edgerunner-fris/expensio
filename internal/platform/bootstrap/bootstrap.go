// Package bootstrap 是六个服务共用的进程外壳。
//
// 六个 main.go 只声明"我是谁、监听哪个端口、需要哪些依赖"，生命周期只有这一份实现：
// 加载配置 → 建日志 → 建健康检查注册表 → 建 HTTP 服务器 → 等信号 → 优雅关闭。
// 这样做的直接好处是：关闭顺序（摘流量 → 排空 → Shutdown）不可能在某个服务里写歪，
// 也不会出现"改了 gateway 忘了改 billing"的双轨维护。
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/health"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/server"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
	"github.com/edgerunner-fris/expensio/internal/platform/version"
)

// App 是装配好的一个服务实例。
// 业务代码通过 Config / Logger / Health / Engine 拿到自己需要的东西，
// 但拿不到 http.Server，避免绕过统一的生命周期。
type App struct {
	Config *config.Config
	Logger *zap.Logger
	Health *health.Registry
	Server *server.Server

	closers []func()
	once    sync.Once
}

// OnStop 注册进程退出前的清理动作（关闭连接池、停止消费者等）。
// 执行顺序与注册顺序相反，与 defer 一致：后建立的依赖先释放。
func (a *App) OnStop(fn func()) {
	if fn == nil {
		return
	}
	a.closers = append(a.closers, fn)
}

// close 只执行一次：setup 失败与正常退出都可能走到这里。
func (a *App) close() {
	a.once.Do(func() {
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
	})
}

// New 按顺序装配一个服务。任何一步失败都立即返回，不做半启动。
func New(opts config.Options) (*App, error) {
	cfg, err := config.Load(opts)
	if err != nil {
		return nil, err
	}

	lg, err := logger.New(cfg.LogLevel, cfg.Service, version.Version)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: 初始化日志失败: %w", err)
	}

	tp, err := telemetry.Setup(context.Background(), telemetry.Config{
		Enabled:  cfg.OTel.Enabled,
		Endpoint: cfg.OTel.Endpoint,
		Insecure: cfg.OTel.Insecure,
		Service:  cfg.Service,
		Version:  version.Version,
		Env:      cfg.Env,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: 初始化遥测失败: %w", err)
	}

	var httpMiddleware []gin.HandlerFunc
	if tp.Enabled() {
		httpMiddleware = append(httpMiddleware, telemetry.GinMiddleware())
	}

	reg := health.New(cfg.Service)

	trustedProxies, err := cfg.HTTP.TrustedProxyList()
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("bootstrap: 解析可信代理失败: %w", err)
	}
	if cfg.Service == "gateway" && len(trustedProxies) == 0 {
		lg.Warn("网关未配置可信代理：经反代的请求会共享同一个来源 IP，按 IP 限流将退化为全站一个桶",
			zap.String("env_var", "EXPENSIO_HTTP_TRUSTED_PROXIES"))
	}

	srv, err := server.New(server.Options{
		Addr:            cfg.HTTP.Addr,
		Mode:            ginMode(cfg.Env),
		ReadTimeout:     cfg.HTTP.ReadTimeout,
		WriteTimeout:    cfg.HTTP.WriteTimeout,
		IdleTimeout:     cfg.HTTP.IdleTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
		DrainDelay:      cfg.HTTP.DrainDelay,
		Logger:          lg,
		Health:          reg,
		Middleware:      httpMiddleware,
		TrustedProxies:  trustedProxies,
	})
	if err != nil {
		_ = tp.Shutdown(context.Background())
		return nil, fmt.Errorf("bootstrap: 初始化 HTTP 服务失败: %w", err)
	}

	// service 与 version 已由 logger 作为固定字段附加，这里不再重复输出，
	// 否则 JSON 里会出现同名键（zap 不校验重复字段，下游解析结果不可预期）。
	lg.Info("config loaded",
		zap.String("env", cfg.Env),
		zap.String("addr", cfg.HTTP.Addr),
		zap.Bool("otel_enabled", cfg.OTel.Enabled),
	)

	app := &App{Config: cfg, Logger: lg, Health: reg, Server: srv}
	// 这里注册得最早，因此最后执行：此时 HTTP 已停止，业务连接池也已关闭，
	// 再把缓冲的 span/metric 刷给 Collector。
	app.OnStop(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			lg.Warn("telemetry shutdown failed", zap.Error(err))
		}
	})
	return app, nil
}

// Main 是可执行文件的统一入口，返回进程退出码。
//
// 用返回值而不是直接 os.Exit，是为了让 defer（日志 Sync、资源释放）照常执行；
// 在 main 里直接 os.Exit 会让这些 defer 全部跳过。
func Main(opts config.Options, setup func(*App) error) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := New(opts)
	if err != nil {
		// 此时还没有可信的 logger，只能写 stderr。
		fmt.Fprintf(os.Stderr, "%s 启动失败: %v\n", opts.Service, err)
		return 1
	}
	defer func() { _ = app.Logger.Sync() }()

	if setup != nil {
		if err := setup(app); err != nil {
			app.Logger.Error("service setup failed", zap.Error(err))
			app.close()
			return 1
		}
	}

	runErr := app.Server.Run(ctx)
	// 先停依赖、再退出：HTTP 已经不再接新请求，此时关闭连接池不会打断任何在用连接。
	app.close()

	if runErr != nil {
		app.Logger.Error("service exited with error", zap.Error(runErr))
		return 1
	}
	app.Logger.Info("service stopped")
	return 0
}

// ginMode 把环境映射成 gin 模式：local 保留调试输出，其余一律 release。
func ginMode(env string) string {
	if env == "local" {
		return "debug"
	}
	return "release"
}
