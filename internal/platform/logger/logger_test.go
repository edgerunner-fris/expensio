package logger

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func newBufferLogger(buf *bytes.Buffer) *zap.Logger {
	encCfg := zap.NewProductionEncoderConfig()
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encCfg),
		zapcore.AddSync(buf),
		zap.DebugLevel,
	)
	return zap.New(redactCore{Core: core})
}

func TestRedactsSensitiveStringFields(t *testing.T) {
	var buf bytes.Buffer
	l := newBufferLogger(&buf)

	l.Info("login",
		zap.String("password", "hunter2"),
		zap.String("access_token", "eyJhbGciOi"),
		zap.String("email", "user@example.com"),
	)

	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "eyJhbGciOi")
	assert.Contains(t, out, `"password":"***"`)
	assert.Contains(t, out, `"access_token":"***"`)
	assert.Contains(t, out, "user@example.com")
}

func TestNonSensitiveFieldsPassThrough(t *testing.T) {
	var buf bytes.Buffer
	l := newBufferLogger(&buf)

	l.Info("expense submitted", zap.String("tenant_id", "t-1"), zap.Int("amount", 1200))

	out := buf.String()
	assert.Contains(t, out, "t-1")
	assert.Contains(t, out, "1200")
}

func TestNewRejectsInvalidLevel(t *testing.T) {
	_, err := New("verbose", "gateway", "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verbose")
}

func TestNewTagsServiceAndVersion(t *testing.T) {
	var buf bytes.Buffer
	l, err := newWithSink("info", "gateway", "v1.2.3", zapcore.AddSync(&buf))
	require.NoError(t, err)
	require.NotNil(t, l)

	l.Info("started")

	out := buf.String()
	assert.Contains(t, out, `"service":"gateway"`)
	assert.Contains(t, out, `"version":"v1.2.3"`)
	assert.Contains(t, out, `"level":"info"`)
}
