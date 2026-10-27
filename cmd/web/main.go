// Command web 提供管理台静态资源并把 /api/ 反向代理到 gateway。
//
// 它刻意不接入 bootstrap：不需要数据库、Redis、Kafka 或 JWT 配置，
// 启动失败只可能是静态资源缺失或网关地址非法，属于部署配置错误。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
	"github.com/edgerunner-fris/expensio/internal/platform/version"
	"github.com/edgerunner-fris/expensio/internal/web"
)

func main() {
	if err := run(); err != nil {
		// logger 可能还没建好，统一写 stderr；容器里由日志采集器收走。
		fmt.Fprintf(os.Stderr, "web 启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	log, err := logger.New(envOr("EXPENSIO_LOG_LEVEL", "info"), "web", version.Version)
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	// web 不接 bootstrap，遥测这里单独装配；配置项与其它服务同名同义。
	tp, err := telemetry.Setup(context.Background(), telemetry.Config{
		Enabled:  envBool("EXPENSIO_OTEL_ENABLED", false),
		Endpoint: envOr("EXPENSIO_OTEL_ENDPOINT", "localhost:4317"),
		Insecure: envBool("EXPENSIO_OTEL_INSECURE", true),
		Service:  "web",
		Version:  version.Version,
		Env:      envOr("EXPENSIO_ENV", "local"),
	})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Warn("telemetry shutdown failed", zap.Error(err))
		}
	}()

	opts := web.Options{
		Root:       envOr("EXPENSIO_WEB_ROOT", "./frontend/dist"),
		GatewayURL: envOr("EXPENSIO_GATEWAY_URL", "http://127.0.0.1:8080"),
		Logger:     log,
		Version:    version.Version,
	}
	handler, err := web.New(opts)
	if err != nil {
		return err
	}
	defer handler.Close()

	addr := envOr("EXPENSIO_WEB_ADDR", ":8086")
	httpHandler := handler.Handler()
	if tp.Enabled() {
		httpHandler = telemetry.HTTPHandler(httpHandler, web.RouteLabel)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Info("web listening",
			zap.String("addr", addr),
			zap.String("root", opts.Root),
			zap.String("gateway", opts.GatewayURL),
			zap.String("version", version.Version))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("web: 监听失败: %w", err)
	case <-ctx.Done():
		log.Info("web shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("web: 优雅关闭失败: %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
