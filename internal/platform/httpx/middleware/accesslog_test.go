package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// AccessLog 里的 trace_id/span_id 是 Loki → Tempo 跳转的唯一依据。
// 失效时日志仍然能看，但"从一行错误日志跳到整条链路"会悄悄断掉，
// 因此专门锁住这个字段。
func TestAccessLogIncludesTraceContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	cases := map[string]struct {
		ctx     func(*http.Request) *http.Request
		traceID string
		spanID  string
	}{
		"有 span 时写入 trace_id 与 span_id": {
			ctx: func(r *http.Request) *http.Request {
				return r.WithContext(trace.ContextWithSpanContext(r.Context(), spanCtx))
			},
			traceID: traceID.String(),
			spanID:  spanID.String(),
		},
		"没有 span 时不写空字段": {
			ctx: func(r *http.Request) *http.Request { return r },
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			engine := gin.New()
			engine.Use(AccessLog(zap.New(core)))
			engine.GET("/api/v1/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
			engine.ServeHTTP(httptest.NewRecorder(), tc.ctx(req))

			entries := logs.FilterMessage("http request").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			if tc.traceID == "" {
				// 没有 span 时字段必须完全缺席，而不是写成空字符串。
				assert.NotContains(t, fields, "trace_id")
				assert.NotContains(t, fields, "span_id")
				return
			}
			assert.Equal(t, tc.traceID, fields["trace_id"])
			assert.Equal(t, tc.spanID, fields["span_id"])
		})
	}
}
