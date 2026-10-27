package kafka

import (
	"context"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
)

const instrumentationName = "github.com/edgerunner-fris/expensio/internal/platform/kafka"

// kafkaDurationBuckets 覆盖 1ms–10s：生产含 broker 确认但通常在几十毫秒内，
// 消费包含业务处理，跨度更大。默认边界从 5s 起步，无法区分正常与慢请求。
var kafkaDurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

var (
	producerRecords = telemetry.NewCounter(
		"expensio.kafka.producer.records",
		"已投递到 Kafka 的记录数（按主题与结果）",
		"{record}",
	)
	producerDuration = telemetry.NewHistogramWithBuckets(
		"expensio.kafka.producer.duration",
		"Kafka 生产请求耗时（含 broker 确认）",
		"s",
		kafkaDurationBuckets...,
	)
	consumerRecords = telemetry.NewCounter(
		"expensio.kafka.consumer.records",
		"已处理的 Kafka 记录数（按主题与结果）",
		"{record}",
	)
	consumerDuration = telemetry.NewHistogramWithBuckets(
		"expensio.kafka.consumer.duration",
		"单条 Kafka 记录从开始处理到提交前的结果耗时",
		"s",
		kafkaDurationBuckets...,
	)
)

// headerCarrier 让 W3C traceparent 能放进 Kafka 记录头。
// Kafka 头是 []byte，大小写敏感；键按小写写入，读取时忽略大小写。
type headerCarrier struct {
	headers []kgo.RecordHeader
}

func (c *headerCarrier) Get(key string) string {
	for _, h := range c.headers {
		if strings.EqualFold(h.Key, key) {
			return string(h.Value)
		}
	}
	return ""
}

func (c *headerCarrier) Set(key, value string) {
	for i := range c.headers {
		if strings.EqualFold(c.headers[i].Key, key) {
			c.headers[i].Value = []byte(value)
			return
		}
	}
	c.headers = append(c.headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

func (c *headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c.headers))
	for _, h := range c.headers {
		keys = append(keys, h.Key)
	}
	return keys
}

// 传播器必须每次读取：Setup 在包初始化之后才替换全局实现，
// 在 init 阶段捕获到的会是 noop 传播器。
func injectHeaders(ctx context.Context, base []kgo.RecordHeader) []kgo.RecordHeader {
	carrier := &headerCarrier{headers: append([]kgo.RecordHeader(nil), base...)}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.headers
}

func extractHeaders(ctx context.Context, headers []kgo.RecordHeader) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, &headerCarrier{headers: headers})
}

func startPublish(ctx context.Context, topic string) (context.Context, trace.Span) {
	return otel.Tracer(instrumentationName).Start(ctx, "kafka.publish "+topic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.operation", "send"),
			attribute.String("messaging.destination.name", topic),
		),
	)
}

func finishPublish(ctx context.Context, span trace.Span, topic string, start time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "kafka publish failed")
	}
	attrs := []attribute.KeyValue{
		attribute.String("messaging.destination.name", topic),
		attribute.String("outcome", outcome),
	}
	producerRecords.Add(ctx, 1, attrs...)
	producerDuration.Record(ctx, time.Since(start).Seconds(), attrs...)
	span.End()
}

func startConsume(ctx context.Context, msg Message) (context.Context, trace.Span) {
	ctx = extractHeaders(ctx, msg.Headers)
	return otel.Tracer(instrumentationName).Start(ctx, "kafka.consume "+msg.Topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.operation", "process"),
			attribute.String("messaging.destination.name", msg.Topic),
			attribute.Int("messaging.kafka.partition", int(msg.Partition)),
			attribute.Int64("messaging.kafka.offset", msg.Offset),
		),
	)
}

func finishConsume(ctx context.Context, span trace.Span, topic string, start time.Time, outcome string) {
	span.SetAttributes(attribute.String("outcome", outcome))
	switch outcome {
	case "dlq":
		span.SetStatus(codes.Error, "message routed to DLQ")
	case "canceled":
		span.SetStatus(codes.Error, "consumer canceled")
	}
	attrs := []attribute.KeyValue{
		attribute.String("messaging.destination.name", topic),
		attribute.String("outcome", outcome),
	}
	consumerRecords.Add(ctx, 1, attrs...)
	consumerDuration.Record(ctx, time.Since(start).Seconds(), attrs...)
	span.End()
}
