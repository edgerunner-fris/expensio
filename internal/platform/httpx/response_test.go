package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
)

func newEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(middleware.RequestID())
	e.Use(middleware.Recovery(zap.NewNop()))
	return e
}

func TestErrorWritesEnvelopeAndStatus(t *testing.T) {
	e := newEngine()
	e.GET("/missing", func(c *gin.Context) {
		httpx.Error(c, apperr.NotFound("费用"))
	})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/missing", nil))

	require.Equal(t, http.StatusNotFound, w.Code)

	var env httpx.Envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, apperr.CodeNotFound, env.Code)
	assert.Contains(t, env.Message, "不存在")
	assert.NotEmpty(t, env.RequestID)
	assert.Equal(t, env.RequestID, w.Header().Get("X-Request-ID"))
}

func TestOKWritesData(t *testing.T) {
	e := newEngine()
	e.GET("/ok", func(c *gin.Context) {
		httpx.OK(c, gin.H{"value": 42})
	})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))

	require.Equal(t, http.StatusOK, w.Code)

	var env struct {
		Code string         `json:"code"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, httpx.CodeOK, env.Code)
	assert.Equal(t, float64(42), env.Data["value"])
}

func TestRequestIDGeneratedWhenMissing(t *testing.T) {
	e := newEngine()
	e.GET("/id", func(c *gin.Context) { httpx.OK(c, nil) })

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/id", nil))

	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
}

func TestRequestIDPreservedWhenValid(t *testing.T) {
	e := newEngine()
	e.GET("/id", func(c *gin.Context) { httpx.OK(c, nil) })

	req := httptest.NewRequest(http.MethodGet, "/id", nil)
	req.Header.Set("X-Request-ID", "trace-abc_123")

	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	assert.Equal(t, "trace-abc_123", w.Header().Get("X-Request-ID"))
}

func TestRequestIDRejectedWhenMalformed(t *testing.T) {
	e := newEngine()
	e.GET("/id", func(c *gin.Context) { httpx.OK(c, nil) })

	req := httptest.NewRequest(http.MethodGet, "/id", nil)
	req.Header.Set("X-Request-ID", "bad id with spaces\ninjected")

	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	got := w.Header().Get("X-Request-ID")
	assert.NotEqual(t, "bad id with spaces\ninjected", got)
	assert.NotEmpty(t, got)
}

func TestRecoveryReturnsInternalErrorWithoutLeakingPanic(t *testing.T) {
	e := newEngine()
	e.GET("/boom", func(c *gin.Context) {
		panic("super-secret-detail")
	})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "super-secret-detail")

	var env httpx.Envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, apperr.CodeInternal, env.Code)
	assert.Equal(t, "服务内部错误，请稍后重试", env.Message)
}
