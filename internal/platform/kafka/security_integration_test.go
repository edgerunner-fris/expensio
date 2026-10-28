package kafka_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// saslSecurity 返回指向测试 broker 的完整安全参数。
func saslSecurity(env *testenv.KafkaSASL) kafka.Security {
	return kafka.Security{
		TLS:           true,
		TLSCAFile:     env.CAFile,
		SASLMechanism: "plain",
		SASLUsername:  env.Username,
		SASLPassword:  env.Password,
	}
}

// TestSASLPlainOverTLSRoundTrip 在真实 SASL_SSL broker 上跑完握手、生产、
// 消费与位点提交：这正是托管 Kafka 的连接形态，不能只在单元测试里拼选项。
func TestSASLPlainOverTLSRoundTrip(t *testing.T) {
	env := testenv.StartKafkaSASL(t)
	sec := saslSecurity(env)

	suffix := uuid.NewString()
	topic := "sasl-roundtrip-" + suffix
	dlqTopic := topic + "-dlq"
	env.CreateTopic(t, topic, 1)
	env.CreateTopic(t, dlqTopic, 1)

	producer, err := kafka.NewProducer(env.Brokers, "sasl-producer-"+suffix, sec)
	require.NoError(t, err)
	t.Cleanup(producer.Close)

	// 先 Ping：TLS 握手与 SASL 认证都在这一步真实发生。
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	require.NoError(t, producer.Ping(pingCtx))
	pingCancel()

	payload := []byte("secure-event-" + suffix)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, producer.Publish(ctx, topic, "tenant-a", payload))

	received := make(chan string, 8)
	consumer, err := kafka.NewConsumer(kafka.ConsumerOptions{
		Brokers:   env.Brokers,
		GroupID:   "sasl-group-" + suffix,
		Topic:     topic,
		DLQTopic:  dlqTopic,
		ClientID:  "sasl-consumer-" + suffix,
		Handler:   kafka.HandlerFunc(func(_ context.Context, msg kafka.Message) error { received <- string(msg.Value); return nil }),
		Publisher: producer,
		Logger:    zap.NewNop(),
		Reset:     kafka.ResetEarliest,
		Security:  sec,
	})
	require.NoError(t, err)
	t.Cleanup(consumer.Close)

	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consumer.Run(runCtx)
	}()

	select {
	case got := <-received:
		require.Equal(t, string(payload), got)
	case <-time.After(60 * time.Second):
		t.Fatal("60 秒内没有通过 SASL_SSL 消费到消息")
	}

	stop()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("取消上下文后消费循环没有退出")
	}
}

// TestSASLWrongPasswordIsRejected 证明 broker 真的在校验口令，
// 而不是"只要配了 SASL 字段就当作已认证"。
func TestSASLWrongPasswordIsRejected(t *testing.T) {
	env := testenv.StartKafkaSASL(t)
	sec := saslSecurity(env)
	sec.SASLPassword = env.Password + "-wrong"

	producer, err := kafka.NewProducer(env.Brokers, "sasl-bad-password-"+uuid.NewString(), sec)
	require.NoError(t, err)
	t.Cleanup(producer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.Error(t, producer.Ping(ctx), "错误口令必须被 broker 拒绝")
}

// TestPlainClientRejectedBySASLBroker 证明 SASL_SSL 监听器不接受明文客户端：
// 否则"配了 TLS 但客户端仍在明文连接"这类回归不会被发现。
func TestPlainClientRejectedBySASLBroker(t *testing.T) {
	env := testenv.StartKafkaSASL(t)

	producer, err := kafka.NewProducer(env.Brokers, "plain-on-sasl-"+uuid.NewString())
	require.NoError(t, err)
	t.Cleanup(producer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.Error(t, producer.Ping(ctx), "明文客户端不能连上 SASL_SSL 监听器")
}
