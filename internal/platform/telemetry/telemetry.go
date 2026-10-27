// Package telemetry 是六个服务共用的 OpenTelemetry 装配。
//
// 设计约束：
//   - Collector 不可用不能阻止服务启动。OTLP 导出器在后台异步重试，
//     Setup 只负责建管线，不在这里做网络探活。
//   - 关闭顺序由 bootstrap 控制：HTTP 停止后再 Shutdown，尽量把缓冲的
//     span/metric 刷出去。
//   - 关闭（Enabled=false）时完全不改全局 provider，测试与本地裸跑保持 noop。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config 是启动遥测所需的全部输入。
type Config struct {
	Enabled  bool
	Endpoint string
	Insecure bool
	Service  string
	Version  string
	Env      string
}

// Providers 持有两个 provider，供进程退出时统一关闭。
type Providers struct {
	tracer *sdktrace.TracerProvider
	meter  *sdkmetric.MeterProvider
}

// Enabled 表示当前进程是否真的产生了遥测数据。
func (p *Providers) Enabled() bool { return p != nil && p.tracer != nil }

// Setup 建立 trace/metric 管线并设置为全局 provider。
//
// 采样策略固定为 ParentBased(AlwaysSample)：本地与演示环境的数据量很小，
// 全采样便于排查；生产若出现高基数压力，应在此处改为可配置的比例采样。
func Setup(ctx context.Context, cfg Config) (*Providers, error) {
	if !cfg.Enabled {
		return &Providers{}, nil
	}
	if cfg.Endpoint == "" {
		return nil, errors.New("telemetry: 启用遥测时 endpoint 不能为空")
	}
	if cfg.Service == "" {
		return nil, errors.New("telemetry: service 不能为空")
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", cfg.Service),
		attribute.String("service.version", cfg.Version),
		attribute.String("deployment.environment.name", cfg.Env),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: 构造 resource 失败: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: 创建 trace 导出器失败: %w", err)
	}
	tracer := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		// 已经创建的 tracer 必须回收，否则后台批处理 goroutine 会泄漏。
		_ = tracer.Shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("telemetry: 创建 metric 导出器失败: %w", err)
	}
	meter := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		// 10s 的导出间隔让本地 Prometheus（15s 抓取）足够平滑，也避免
		// 每条请求都触发一次网络写。
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(10*time.Second))),
	)

	// 恒定值 1 的 build_info 是"这个服务还活着并且指标链路通"的最小信号：
	// 没有它，一个零流量的服务在 Prometheus 里完全不存在，告警无从下手。
	m := meter.Meter(instrumentationName)
	buildInfo, err := m.Int64ObservableGauge("expensio.build_info",
		metric.WithDescription("服务启动即存在的常量指标，用于存活与版本识别"))
	if err != nil {
		_ = meter.Shutdown(context.WithoutCancel(ctx))
		_ = tracer.Shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("telemetry: 创建 build_info 失败: %w", err)
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(buildInfo, 1)
		return nil
	}, buildInfo); err != nil {
		_ = meter.Shutdown(context.WithoutCancel(ctx))
		_ = tracer.Shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("telemetry: 注册 build_info 回调失败: %w", err)
	}

	otel.SetTracerProvider(tracer)
	otel.SetMeterProvider(meter)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Providers{tracer: tracer, meter: meter}, nil
}

// Shutdown 先刷指标再刷 trace；返回所有错误，调用方只记录、不阻断退出。
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.meter != nil {
		errs = append(errs, p.meter.Shutdown(ctx))
		p.meter = nil
	}
	if p.tracer != nil {
		errs = append(errs, p.tracer.Shutdown(ctx))
		p.tracer = nil
	}
	return errors.Join(errs...)
}
