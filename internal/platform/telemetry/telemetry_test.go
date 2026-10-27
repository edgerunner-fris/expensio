package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newTestProviders 把全局 provider 换成内存实现，并在测试结束后还原，
// 避免污染同包其它测试或其它包偶然使用全局 tracer。
func newTestProviders(t *testing.T) (*sdkmetric.ManualReader, *tracetest.InMemoryExporter) {
	t.Helper()

	prevTracer := otel.GetTracerProvider()
	prevMeter := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTracer)
		otel.SetMeterProvider(prevMeter)
		otel.SetTextMapPropagator(prevProp)
	})

	exporter := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))

	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return reader, exporter
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("未找到指标 %s", name)
	return metricdata.Metrics{}
}

func attrValue(t *testing.T, set attribute.Set, key string) attribute.Value {
	t.Helper()
	v, ok := set.Value(attribute.Key(key))
	require.Truef(t, ok, "属性 %s 不存在", key)
	return v
}

func TestGinMiddlewareRecordsSpanAndMetrics(t *testing.T) {
	reader, exporter := newTestProviders(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/hello/:id", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hello/42", nil))
	require.Equal(t, http.StatusOK, w.Code)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "GET /hello/:id", span.Name)
	assert.Equal(t, trace.SpanKindServer, span.SpanKind)

	var route, status string
	for _, kv := range span.Attributes {
		switch kv.Key {
		case "http.route":
			route = kv.Value.AsString()
		case "http.response.status_code":
			status = kv.Value.Emit()
		}
	}
	assert.Equal(t, "/hello/:id", route)
	assert.Equal(t, "200", status)

	rm := collect(t, reader)

	requests := findMetric(t, rm, metricHTTPRequests)
	sum, ok := requests.Data.(metricdata.Sum[int64])
	require.True(t, ok, "requests 应为 Sum[int64]，实际 %T", requests.Data)
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, int64(1), sum.DataPoints[0].Value)
	assert.Equal(t, "/hello/:id", attrValue(t, sum.DataPoints[0].Attributes, "http.route").AsString())
	assert.Equal(t, int64(200), attrValue(t, sum.DataPoints[0].Attributes, "http.response.status_code").AsInt64())

	duration := findMetric(t, rm, metricHTTPDuration)
	hist, ok := duration.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "duration 应为 Histogram[float64]，实际 %T", duration.Data)
	require.Len(t, hist.DataPoints, 1)
	assert.Equal(t, uint64(1), hist.DataPoints[0].Count)
	// 默认边界从 5s 起，P95 会失去分辨率；这里锁定亚秒级边界。
	assert.Equal(t, httpDurationBuckets, hist.DataPoints[0].Bounds)

	active := findMetric(t, rm, metricHTTPActive)
	updown, ok := active.Data.(metricdata.Sum[int64])
	require.True(t, ok, "active 应为 Sum[int64]，实际 %T", active.Data)
	require.Len(t, updown.DataPoints, 1)
	assert.Equal(t, int64(0), updown.DataPoints[0].Value)
}

func TestGinMiddlewareSkipsHealth(t *testing.T) {
	reader, exporter := newTestProviders(t)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Empty(t, exporter.GetSpans())
	rm := collect(t, reader)
	for _, sm := range rm.ScopeMetrics {
		assert.Empty(t, sm.Metrics, "健康检查不应产生指标")
	}
}

func TestGinMiddlewareContinuesTraceparent(t *testing.T) {
	_, exporter := newTestProviders(t)

	parentCtx, parent := otel.Tracer("test").Start(context.Background(), "upstream")
	parentTraceID := parent.SpanContext().TraceID()
	parentSpanID := parent.SpanContext().SpanID()
	defer parent.End()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/hello", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	otel.GetTextMapPropagator().Inject(parentCtx, propagation.HeaderCarrier(req.Header))
	r.ServeHTTP(httptest.NewRecorder(), req)

	spans := exporter.GetSpans()
	var child *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "GET /hello" {
			child = &spans[i]
			break
		}
	}
	require.NotNil(t, child, "未找到子 span")
	assert.Equal(t, parentTraceID, child.SpanContext.TraceID())
	assert.Equal(t, parentSpanID, child.Parent.SpanID())
}

func TestHTTPHandlerRecordsSpanAndMetrics(t *testing.T) {
	reader, exporter := newTestProviders(t)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})
	h := HTTPHandler(next, func(*http.Request) string { return "api" })

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/anything", nil))
	require.Equal(t, http.StatusCreated, w.Code)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "POST api", spans[0].Name)
	for _, kv := range spans[0].Attributes {
		if kv.Key == "http.response.status_code" {
			assert.Equal(t, int64(201), kv.Value.AsInt64())
		}
	}

	rm := collect(t, reader)
	requests := findMetric(t, rm, metricHTTPRequests)
	sum, ok := requests.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 1)
	assert.Equal(t, int64(1), sum.DataPoints[0].Value)
	assert.Equal(t, "api", attrValue(t, sum.DataPoints[0].Attributes, "http.route").AsString())
	assert.Equal(t, int64(201), attrValue(t, sum.DataPoints[0].Attributes, "http.response.status_code").AsInt64())
}

func TestHTTPHandlerSkipsUnlabeledRequests(t *testing.T) {
	_, exporter := newTestProviders(t)

	h := HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), func(*http.Request) string { return "" })

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	assert.Empty(t, exporter.GetSpans())
}

func TestSetupDisabledIsNoop(t *testing.T) {
	providers, err := Setup(context.Background(), Config{Enabled: false})
	require.NoError(t, err)
	assert.False(t, providers.Enabled())
	assert.NoError(t, providers.Shutdown(context.Background()))
}

func TestSetupRejectsEmptyEndpoint(t *testing.T) {
	_, err := Setup(context.Background(), Config{Enabled: true, Service: "gateway", Endpoint: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}
