package db

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const dbTelemetryName = "github.com/edgerunner-fris/expensio/internal/platform/db"

// RegisterMetrics 把 pgxpool 的实时状态暴露为可观测指标。
//
// pgxpool 没有内置的 OTel 导出，而这些数字恰好是排查"连接耗尽"最快的入口：
// acquired 接近 max 时，请求排队的原因就不再是数据库慢，而是池太小或泄漏。
// 注册失败只影响可观测性，因此调用方不应把它当作启动失败。
func (d *DB) RegisterMetrics() error {
	if d == nil || d.pool == nil {
		return nil
	}
	meter := otel.Meter(dbTelemetryName)

	acquired, err := meter.Int64ObservableGauge("expensio.db.pool.acquired_conns",
		metric.WithDescription("当前被借出的连接数"))
	if err != nil {
		return err
	}
	idle, err := meter.Int64ObservableGauge("expensio.db.pool.idle_conns",
		metric.WithDescription("当前空闲连接数"))
	if err != nil {
		return err
	}
	total, err := meter.Int64ObservableGauge("expensio.db.pool.total_conns",
		metric.WithDescription("池内连接总数（含正在建立）"))
	if err != nil {
		return err
	}
	maxConns, err := meter.Int64ObservableGauge("expensio.db.pool.max_conns",
		metric.WithDescription("池允许的最大连接数"))
	if err != nil {
		return err
	}
	acquireCount, err := meter.Int64ObservableCounter("expensio.db.pool.acquires",
		metric.WithDescription("累计借出连接次数（含命中空闲连接）"))
	if err != nil {
		return err
	}

	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s := d.pool.Stat()
		o.ObserveInt64(acquired, int64(s.AcquiredConns()))
		o.ObserveInt64(idle, int64(s.IdleConns()))
		o.ObserveInt64(total, int64(s.TotalConns()))
		o.ObserveInt64(maxConns, int64(s.MaxConns()))
		o.ObserveInt64(acquireCount, s.AcquireCount())
		return nil
	}, acquired, idle, total, maxConns, acquireCount)
	return err
}
