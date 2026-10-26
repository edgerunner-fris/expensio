// Package kafka 封装事件总线的生产者。
//
// 只做一件事：把 outbox 里的一行字节原样发到指定主题。
// 分区键由调用方给出（当前固定为 tenant_id），这样同一租户的事件
// 会落到同一分区，消费端看到的顺序与发送顺序一致。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Producer 是并发安全的：franz-go 的 Client 本身支持多 goroutine 复用。
type Producer struct {
	client *kgo.Client
}

// NewProducer 建立客户端。构造成功不代表 broker 可达：
// Kafka 故障不应阻止 expense 启动，投递器会在后台持续重试。
func NewProducer(brokers []string, clientID string) (*Producer, error) {
	if len(brokers) == 0 {
		return nil, errors.New("kafka: brokers 不能为空")
	}
	if clientID == "" {
		return nil, errors.New("kafka: clientID 不能为空")
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		// 全量 ISR 确认：宁可等，也不要把"可能丢失"的事件当成已投递。
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// 幂等写是 franz-go 的默认行为（除非显式 DisableIdempotentWrite）：
		// broker 重试不会在分区内产生重复。跨进程崩溃仍可能重复
		// （已发送未标记），消费端必须幂等。
		kgo.ProducerLinger(5*time.Millisecond),
		kgo.RecordRetries(5),
		kgo.RecordDeliveryTimeout(10*time.Second),
		kgo.MaxBufferedRecords(4096),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: 创建客户端失败: %w", err)
	}
	return &Producer{client: client}, nil
}

// Ping 用于启动时探测 broker；失败只应记录日志，不应终止进程。
func (p *Producer) Ping(ctx context.Context) error {
	return p.client.Ping(ctx)
}

// Publish 同步等待 broker 确认。返回 nil 表示事件已经进入 ISR，
// 此时 outbox 才能标记为已发布。
func (p *Producer) Publish(ctx context.Context, topic, key string, value []byte) error {
	return p.PublishRecord(ctx, &kgo.Record{
		Topic: topic,
		Key:   []byte(key),
		Value: value,
	})
}

// PublishRecord 发送完整记录（含 headers）。消费端写 DLQ 时用它保留
// 原始字节与来源信息；outbox 投递仍然走 Publish。
func (p *Producer) PublishRecord(ctx context.Context, rec *kgo.Record) error {
	return p.client.ProduceSync(ctx, rec).FirstErr()
}

func (p *Producer) Close() { p.client.Close() }
