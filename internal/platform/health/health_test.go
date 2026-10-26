package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/health"
)

func newEngine(reg *health.Registry) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/healthz", reg.Live)
	e.GET("/readyz", reg.Ready)
	return e
}

func TestLiveIgnoresDependencies(t *testing.T) {
	reg := health.New("test")
	reg.Add(health.Check("db", func(context.Context) error {
		return errors.New("db down")
	}))

	w := httptest.NewRecorder()
	newEngine(reg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReadyOKWhenAllChecksPass(t *testing.T) {
	reg := health.New("test")
	reg.Add(health.Check("db", func(context.Context) error { return nil }))
	reg.Add(health.Check("redis", func(context.Context) error { return nil }))

	w := httptest.NewRecorder()
	newEngine(reg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Status  string            `json:"status"`
		Service string            `json:"service"`
		Version string            `json:"version"`
		Checks  map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "test", body.Service)
	assert.NotEmpty(t, body.Version)
	assert.Equal(t, "ok", body.Checks["db"])
}

func TestReadyFailsFastWhenAnyCheckFails(t *testing.T) {
	reg := health.New("test")
	reg.Add(health.Check("db", func(context.Context) error { return nil }))
	reg.Add(health.Check("kafka", func(context.Context) error { return errors.New("boom") }))

	w := httptest.NewRecorder()
	newEngine(reg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "unavailable", body.Status)
	assert.Equal(t, "boom", body.Checks["kafka"])
	assert.Equal(t, "ok", body.Checks["db"])
}

func TestReadyWithoutCheckersIsReady(t *testing.T) {
	reg := health.New("test")

	w := httptest.NewRecorder()
	newEngine(reg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusOK, w.Code)
}
