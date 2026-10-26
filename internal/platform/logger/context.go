package logger

import (
	"context"

	"go.uber.org/zap"
)

type ctxKey struct{}

// WithContext 把带请求级字段（request_id / tenant_id）的日志器放入 ctx。
func WithContext(ctx context.Context, l *zap.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext 取出请求级日志器；没有时返回一个丢弃所有日志的 no-op 实例，
// 调用方不需要判断 nil。
func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.NewNop()
}
