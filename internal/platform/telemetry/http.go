package telemetry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/edgerunner-fris/expensio"

	// 指标名统一加 expensio. 前缀，避免与 Collector/运行时的通用指标重名。
	metricHTTPRequests = "expensio.http.server.requests"
	metricHTTPDuration = "expensio.http.server.duration"
	metricHTTPActive   = "expensio.http.server.active_requests"
)

// httpDurationBuckets 覆盖 5ms–10s。OTel 默认边界从 5s 起跳，
// 对本项目这种毫秒级接口等于没有分辨率，P95 只会落在 0 或 5s 上。
var httpDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type httpInstruments struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
}

func newHTTPInstruments(m metric.Meter) (httpInstruments, error) {
	requests, err := m.Int64Counter(metricHTTPRequests,
		metric.WithDescription("按路由与状态码统计的 HTTP 请求数"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return httpInstruments{}, err
	}
	duration, err := m.Float64Histogram(metricHTTPDuration,
		metric.WithDescription("HTTP 请求处理耗时"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(httpDurationBuckets...),
	)
	if err != nil {
		return httpInstruments{}, err
	}
	active, err := m.Int64UpDownCounter(metricHTTPActive,
		metric.WithDescription("正在处理的 HTTP 请求数"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return httpInstruments{}, err
	}
	return httpInstruments{requests: requests, duration: duration, active: active}, nil
}

// instruments 返回当前全局 MeterProvider 下的指标句柄。
// 指标名是常量，正常不会失败；万一失败就退回 noop，绝不让遥测影响业务。
func instruments() httpInstruments {
	inst, err := newHTTPInstruments(otel.Meter(instrumentationName))
	if err == nil {
		return inst
	}
	inst, _ = newHTTPInstruments(noop.NewMeterProvider().Meter(instrumentationName))
	return inst
}

func isHealthPath(path string) bool {
	return path == "/healthz" || path == "/readyz"
}

func requestAttrs(route, method string, status int) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.String("http.route", route),
		attribute.Int("http.response.status_code", status),
	}
}

// GinMiddleware 为每个请求建立 server span，并记录请求数/耗时/并发数。
//
// 必须放在业务中间件之前、RequestID 之外：这样 Recovery 写回的 500 能被
// 计入指标，且 trace 覆盖整条处理链。健康检查不产生 span，避免探针刷屏。
func GinMiddleware() gin.HandlerFunc {
	inst := instruments()
	tracer := otel.Tracer(instrumentationName)
	propagator := otel.GetTextMapPropagator()

	return func(c *gin.Context) {
		route := c.FullPath()
		if isHealthPath(c.Request.URL.Path) {
			c.Next()
			return
		}

		method := c.Request.Method
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		spanName := method + " unmatched"
		if route != "" {
			spanName = method + " " + route
		}
		ctx, span := tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindServer))
		c.Request = c.Request.WithContext(ctx)

		activeAttrs := metric.WithAttributes(attribute.String("http.request.method", method))
		inst.active.Add(ctx, 1, activeAttrs)
		defer inst.active.Add(ctx, -1, activeAttrs)

		start := time.Now()
		c.Next()

		label := route
		if label == "" {
			label = "unmatched"
		}
		status := c.Writer.Status()
		attrs := requestAttrs(label, method, status)
		inst.requests.Add(ctx, 1, metric.WithAttributes(attrs...))
		inst.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))

		span.SetAttributes(attrs...)
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", status))
		}
		span.End()
	}
}

// RouteLabel 根据请求返回低基数路由标签。
// 返回空字符串表示该请求不产生 span/指标（例如静态资源与健康检查）。
type RouteLabel func(*http.Request) string

// HTTPHandler 包装标准库 handler，语义与 GinMiddleware 一致。
// 管理台是 net/http + ReverseProxy，无法复用 Gin 中间件，因此单独提供一份。
func HTTPHandler(next http.Handler, label RouteLabel) http.Handler {
	inst := instruments()
	tracer := otel.Tracer(instrumentationName)
	propagator := otel.GetTextMapPropagator()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := "http"
		if label != nil {
			route = label(r)
		}
		if route == "" || isHealthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		method := r.Method
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		activeAttrs := metric.WithAttributes(attribute.String("http.request.method", method))
		inst.active.Add(ctx, 1, activeAttrs)
		defer inst.active.Add(ctx, -1, activeAttrs)

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r.WithContext(ctx))

		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := requestAttrs(route, method, status)
		inst.requests.Add(ctx, 1, metric.WithAttributes(attrs...))
		inst.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))

		span.SetAttributes(attrs...)
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", status))
		}
	})
}

// statusWriter 捕获状态码，同时保留 ReverseProxy 依赖的 Flush 能力。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能继续控制底层连接。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
