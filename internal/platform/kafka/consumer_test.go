package kafka

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

type stubPublisher struct{}

func (stubPublisher) PublishRecord(context.Context, *kgo.Record) error { return nil }

type stubHandler struct{}

func (stubHandler) Handle(context.Context, Message) error { return nil }

func validOptions() ConsumerOptions {
	return ConsumerOptions{
		Brokers:   []string{"127.0.0.1:1"},
		GroupID:   "g",
		Topic:     "t",
		DLQTopic:  "d",
		ClientID:  "c",
		Handler:   stubHandler{},
		Publisher: stubPublisher{},
		Logger:    zap.NewNop(),
	}
}

func TestPermanentClassification(t *testing.T) {
	assert.Nil(t, Permanent(nil))

	base := errors.New("坏消息")
	assert.False(t, IsPermanent(base))

	permanent := Permanent(base)
	assert.True(t, IsPermanent(permanent))
	assert.ErrorIs(t, permanent, base)

	// 经过一次包装（例如 fmt.Errorf("%w")）仍然是永久错误。
	wrapped := fmt.Errorf("处理失败: %w", permanent)
	assert.True(t, IsPermanent(wrapped))

	// 重复标记不改变错误链。
	assert.Equal(t, permanent, Permanent(permanent))
}

func TestNextBackoff(t *testing.T) {
	base := 500 * time.Millisecond
	assert.Equal(t, time.Second, nextBackoff(base, time.Minute))
	assert.Equal(t, 2*time.Second, nextBackoff(time.Second, time.Minute))
	assert.Equal(t, time.Minute, nextBackoff(40*time.Second, time.Minute), "到达上限后保持不变")
}

func TestNewConsumerValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConsumerOptions)
		want   string
	}{
		{"缺少 brokers", func(o *ConsumerOptions) { o.Brokers = nil }, "brokers"},
		{"缺少 group", func(o *ConsumerOptions) { o.GroupID = "" }, "groupID"},
		{"缺少 topic", func(o *ConsumerOptions) { o.Topic = "" }, "groupID"},
		{"缺少 clientID", func(o *ConsumerOptions) { o.ClientID = "" }, "clientID"},
		{"缺少 handler", func(o *ConsumerOptions) { o.Handler = nil }, "handler"},
		{"缺少 publisher", func(o *ConsumerOptions) { o.Publisher = nil }, "publisher"},
		{"缺少 logger", func(o *ConsumerOptions) { o.Logger = nil }, "logger"},
		{"退避上限小于基数", func(o *ConsumerOptions) {
			o.RetryBase = time.Second
			o.RetryMax = time.Millisecond
		}, "retryMax"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := validOptions()
			tt.mutate(&opts)
			_, err := NewConsumer(opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNewConsumerDefaults(t *testing.T) {
	// kgo.NewClient 不会在构造时连接 broker，因此这里不需要真实 Kafka。
	c, err := NewConsumer(validOptions())
	require.NoError(t, err)
	t.Cleanup(c.Close)

	assert.Equal(t, 500*time.Millisecond, c.retryBase)
	assert.Equal(t, 30*time.Second, c.retryMax)
	assert.Equal(t, "d", c.dlqTopic)
}
