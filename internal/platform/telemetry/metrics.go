package telemetry

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Counter / Histogram 把 instrument 的创建推迟到第一次使用。
//
// 这不是过早优化：bootstrap 在构造业务组件之前才调用 Setup，而包级变量
// 在 init 阶段就会执行。若在 init 里创建 instrument，拿到的永远是当时
// 的 noop provider，之后即使 Setup 也补不回来。
type Counter struct {
	name        string
	description string
	unit        string

	once sync.Once
	inst metric.Int64Counter
}

func NewCounter(name, description, unit string) *Counter {
	return &Counter{name: name, description: description, unit: unit}
}

func (c *Counter) Add(ctx context.Context, delta int64, attrs ...attribute.KeyValue) {
	c.once.Do(func() {
		inst, err := otel.Meter(instrumentationName).Int64Counter(c.name,
			metric.WithDescription(c.description),
			metric.WithUnit(c.unit),
		)
		if err != nil {
			inst, _ = noop.NewMeterProvider().Meter(instrumentationName).Int64Counter(c.name)
		}
		c.inst = inst
	})
	c.inst.Add(ctx, delta, metric.WithAttributes(attrs...))
}

type Histogram struct {
	name        string
	description string
	unit        string
	buckets     []float64

	once sync.Once
	inst metric.Float64Histogram
}

func NewHistogram(name, description, unit string) *Histogram {
	return &Histogram{name: name, description: description, unit: unit}
}

// NewHistogramWithBuckets 与 NewHistogram 相同，但显式指定边界。
// 不指定时 SDK 会使用默认边界（0、5、10…秒），对亚秒级操作没有分辨率。
func NewHistogramWithBuckets(name, description, unit string, buckets ...float64) *Histogram {
	return &Histogram{name: name, description: description, unit: unit, buckets: buckets}
}

func (h *Histogram) Record(ctx context.Context, value float64, attrs ...attribute.KeyValue) {
	h.once.Do(func() {
		opts := []metric.Float64HistogramOption{
			metric.WithDescription(h.description),
			metric.WithUnit(h.unit),
		}
		if len(h.buckets) > 0 {
			opts = append(opts, metric.WithExplicitBucketBoundaries(h.buckets...))
		}
		inst, err := otel.Meter(instrumentationName).Float64Histogram(h.name, opts...)
		if err != nil {
			inst, _ = noop.NewMeterProvider().Meter(instrumentationName).Float64Histogram(h.name)
		}
		h.inst = inst
	})
	h.inst.Record(ctx, value, metric.WithAttributes(attrs...))
}
