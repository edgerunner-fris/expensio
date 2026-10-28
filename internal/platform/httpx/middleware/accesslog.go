package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// probePaths 上的访问日志会淹没真正的业务日志——K8s 每几秒探一次。
var probePaths = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
	"/metrics": {},
}

// AccessLog 记录每个请求的 RED 指标来源字段，并把请求级 logger 注入 ctx。
func AccessLog(base *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		l := base.With(zap.String("request_id", httpx.RequestID(c)))

		// trace_id 必须在本中间件里取：telemetry.GinMiddleware 挂在外层，
		// 此时 span 已经写进了请求上下文。有了这两个字段，Loki 里的
		// 一行日志可以直接跳到 Tempo 的整条链路，而不必靠时间戳猜。
		if sc := trace.SpanFromContext(c.Request.Context()).SpanContext(); sc.IsValid() {
			l = l.With(
				zap.String("trace_id", sc.TraceID().String()),
				zap.String("span_id", sc.SpanID().String()),
			)
		}

		// 下游 handler 通过 logger.FromContext 拿到带 request_id / trace_id 的实例。
		c.Request = c.Request.WithContext(logger.WithContext(c.Request.Context(), l))

		c.Next()

		if _, skip := probePaths[path]; skip {
			return
		}

		status := c.Writer.Status()
		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.Int("status", status),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
			zap.Int("bytes", c.Writer.Size()),
		}

		if t, ok := tenantctx.From(c.Request.Context()); ok {
			fields = append(fields,
				zap.String("tenant_id", t.ID.String()),
				zap.String("user_id", t.UserID.String()),
			)
		}

		if status >= 500 && len(c.Errors) > 0 {
			fields = append(fields, zap.String("error", c.Errors.String()))
		}

		switch {
		case status >= 500:
			l.Error("http request", fields...)
		case status >= 400:
			l.Warn("http request", fields...)
		default:
			l.Info("http request", fields...)
		}
	}
}
