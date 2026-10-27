package kafka_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// TestResetLatestSkipsRecordsWrittenBeforeGroupExists 验证通知消费端的取舍：
// 全新消费组从最新位点开始，保留期内的历史事件不会被补发；组成立之后写入的
// 事件仍然能正常消费。已有位点的重启不受 Reset 影响，由其他用例覆盖。
func TestResetLatestSkipsRecordsWrittenBeforeGroupExists(t *testing.T) {
	k := testenv.StartKafka(t)

	suffix := uuid.NewString()
	topic := "reset-latest-" + suffix
	dlqTopic := topic + "-dlq"
	k.CreateTopic(t, topic, 1)
	k.CreateTopic(t, dlqTopic, 1)

	producer, err := kafka.NewProducer(k.Brokers, "reset-latest-producer-"+suffix)
	require.NoError(t, err)
	t.Cleanup(producer.Close)

	ctx := context.Background()
	// 这条记录早于消费组存在，ResetLatest 必须跳过它。
	require.NoError(t, producer.Publish(ctx, topic, "tenant", []byte("old")))

	received := make(chan string, 16)
	consumer, err := kafka.NewConsumer(kafka.ConsumerOptions{
		Brokers:   k.Brokers,
		GroupID:   "reset-latest-group-" + suffix,
		Topic:     topic,
		DLQTopic:  dlqTopic,
		ClientID:  "reset-latest-consumer-" + suffix,
		Handler:   kafka.HandlerFunc(func(_ context.Context, msg kafka.Message) error { received <- string(msg.Value); return nil }),
		Publisher: producer,
		Logger:    zap.NewNop(),
		Reset:     kafka.ResetLatest,
		RetryBase: 50 * time.Millisecond,
		RetryMax:  200 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(consumer.Close)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = consumer.Run(runCtx) }()

	// 反复写入新记录，直到消费组完成分配并读到其中一条；这样不需要
	// 猜测再平衡耗时，也不会因为第一条新记录早于组分配而误判。
	deadline := time.Now().Add(30 * time.Second)
	for {
		require.NoError(t, producer.Publish(ctx, topic, "tenant", []byte("new")))
		select {
		case value := <-received:
			require.Equal(t, "new", value, "消费组不应读到组成立之前的历史记录")
			// 再等一会儿，确认历史记录不会紧接着被投递（重复的 new 允许）。
			select {
			case extra := <-received:
				require.False(t, strings.HasPrefix(extra, "old"),
					"ResetLatest 之后仍收到了历史记录 %q", extra)
			case <-time.After(time.Second):
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("30 秒内没有消费到消费组成立之后写入的记录")
		}
	}
}
