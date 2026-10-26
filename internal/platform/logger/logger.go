// Package logger 构造结构化日志器，并在写入前对敏感字段打码。
//
// 打码放在 Core 层而不是调用方：只要有一条日志路径忘了脱敏，密码就会进日志系统，
// 而日志系统通常比数据库更容易被下载和检索。
package logger

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// sensitiveFragments 命中即判定为敏感字段（小写包含匹配）。
var sensitiveFragments = []string{
	"password", "passwd", "secret", "token",
	"authorization", "cookie", "card", "cvv", "apikey",
}

// New 返回带 service / version 字段的日志器。
// 输出固定为 JSON：日志采集（Loki、ELK）不依赖正则去猜格式。
func New(level, service, version string) (*zap.Logger, error) {
	return newWithSink(level, service, version, zapcore.Lock(os.Stderr))
}

func newWithSink(level, service, version string, sink zapcore.WriteSyncer) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("logger: 非法日志级别 %q: %w", level, err)
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.MessageKey = "msg"
	encCfg.LevelKey = "level"
	encCfg.NameKey = "logger"
	encCfg.CallerKey = "caller"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeDuration = zapcore.StringDurationEncoder
	encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encCfg),
		sink,
		zap.NewAtomicLevelAt(lvl),
	)

	return zap.New(redactCore{Core: core},
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
	).With(
		zap.String("service", service),
		zap.String("version", version),
	), nil
}

// redactCore 在字段落盘前替换敏感值。
type redactCore struct {
	zapcore.Core
}

func (c redactCore) With(fields []zapcore.Field) zapcore.Core {
	return redactCore{Core: c.Core.With(redactFields(fields))}
}

func (c redactCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c redactCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(ent, redactFields(fields))
}

func redactFields(fields []zapcore.Field) []zapcore.Field {
	out := make([]zapcore.Field, len(fields))
	copy(out, fields)
	for i := range out {
		if out[i].Type == zapcore.StringType && isSensitive(out[i].Key) {
			out[i] = zap.String(out[i].Key, "***")
		}
	}
	return out
}

func isSensitive(key string) bool {
	lower := strings.ToLower(key)
	for _, frag := range sensitiveFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
