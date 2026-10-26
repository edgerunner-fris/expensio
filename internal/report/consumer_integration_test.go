package report

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

var statusToType = map[string]events.Type{
	"submitted":  events.TypeExpenseSubmitted,
	"approved":   events.TypeExpenseApproved,
	"rejected":   events.TypeExpenseRejected,
	"cancelled":  events.TypeExpenseCancelled,
	"reimbursed": events.TypeExpenseReimbursed,
}

type factRow struct {
	Status      string
	Category    string
	Currency    string
	AmountMinor int64
	Version     int
	LastEventID uuid.UUID
}

// harness 是一套真实的 PostgreSQL + Kafka + 消费者。
type harness struct {
	database *db.DB
	producer *kafka.Producer
	consumer *Consumer
	brokers  []string
	topic    string
	dlqTopic string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	pg := testenv.StartPostgres(t, "report")
	database, err := db.Open(context.Background(), pg.AppDSN("report"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	kafkaEnv := testenv.StartKafka(t)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	topic := events.TopicExpense + ".t" + suffix
	dlqTopic := events.TopicExpenseDLQ + ".t" + suffix
	// 测试 broker 关闭了 auto-create：主题与分区数必须显式确定，
	// 否则消费行为取决于第一次 produce 的时序。
	kafkaEnv.CreateTopic(t, topic, 6)
	kafkaEnv.CreateTopic(t, dlqTopic, 1)

	producer, err := kafka.NewProducer(kafkaEnv.Brokers, "report-test")
	require.NoError(t, err)

	consumer, err := NewConsumer(ConsumerOptions{
		Database:     database,
		Brokers:      kafkaEnv.Brokers,
		Publisher:    producer,
		Logger:       zap.NewNop(),
		ClientID:     "report-test",
		Topic:        topic,
		GroupID:      events.ConsumerReportExpense + "-" + suffix,
		DLQTopic:     dlqTopic,
		ConsumerName: events.ConsumerReportExpense,
		RetryBase:    50 * time.Millisecond,
	})
	require.NoError(t, err)

	h := &harness{
		database: database,
		producer: producer,
		consumer: consumer,
		brokers:  kafkaEnv.Brokers,
		topic:    topic,
		dlqTopic: dlqTopic,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if runErr := consumer.Run(ctx); runErr != nil {
			t.Logf("consumer stopped with error: %v", runErr)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("消费者未在 20 秒内停止")
		}
		// 先停消费循环，再关客户端与生产者：DLQ 写入不能撞上已关闭的客户端。
		consumer.Close()
		producer.Close()
	})

	return h
}

func (h *harness) publish(t *testing.T, env events.Envelope) {
	t.Helper()
	raw, err := env.Encode()
	require.NoError(t, err)
	require.NoError(t, h.producer.Publish(context.Background(), h.topic, env.PublishKey(), raw),
		"发布事件失败")
}

func (h *harness) publishRaw(t *testing.T, key string, raw []byte) {
	t.Helper()
	require.NoError(t, h.producer.Publish(context.Background(), h.topic, key, raw))
}

func (h *harness) fact(tenantID, expenseID uuid.UUID) (factRow, bool, error) {
	var f factRow
	var found bool
	err := h.database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			scanErr := tx.QueryRow(ctx, `
				SELECT status, category, currency, amount_minor, version, last_event_id
				FROM app.expense_facts
				WHERE tenant_id = $1 AND expense_id = $2`, tenantID, expenseID).
				Scan(&f.Status, &f.Category, &f.Currency, &f.AmountMinor, &f.Version, &f.LastEventID)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil
			}
			if scanErr != nil {
				return scanErr
			}
			found = true
			return nil
		})
	return f, found, err
}

func (h *harness) processedCount(tenantID uuid.UUID) (int, error) {
	var n int
	err := h.database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM app.processed_events WHERE tenant_id = $1 AND consumer = $2`,
				tenantID, events.ConsumerReportExpense).Scan(&n)
		})
	return n, err
}

func mustEnvelope(t *testing.T, tenantID uuid.UUID, snap events.ExpenseSnapshot) events.Envelope {
	t.Helper()
	payload, err := json.Marshal(snap)
	require.NoError(t, err)
	typ, ok := statusToType[snap.Status]
	require.True(t, ok, "状态 %s 没有对应的事件类型", snap.Status)
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             typ,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         tenantID,
		AggregateID:      snap.ID,
		AggregateVersion: snap.Version,
		OccurredAt:       snap.UpdatedAt,
		Payload:          payload,
	}
}

func snapshot(tenantID, expenseID, submitterID uuid.UUID, status string, version int) events.ExpenseSnapshot {
	return events.ExpenseSnapshot{
		ID:          expenseID,
		TenantID:    tenantID,
		SubmitterID: submitterID,
		Status:      status,
		Category:    "travel",
		Currency:    "USD",
		AmountMinor: 12500,
		OccurredAt:  testClock,
		Version:     version,
		CreatedAt:   testClock,
		UpdatedAt:   testClock.Add(time.Duration(version) * time.Minute),
	}
}

// eventually 轮询直到条件成立。DB 轮询比 sleep 固定时长可靠：
// 事件链路的耗时取决于 Kafka 分批、再平衡与事务，不能靠猜。
func eventually(t *testing.T, desc string, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		ok, err := fn()
		if ok {
			return
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("等待 %s 超时，最后一次错误: %v", desc, lastErr)
			}
			t.Fatalf("等待 %s 超时", desc)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestConsumerAppliesEventsAndDeduplicates(t *testing.T) {
	h := newHarness(t)
	tenantID, expenseID, submitterID := uuid.New(), uuid.New(), uuid.New()

	submitted := snapshot(tenantID, expenseID, submitterID, "submitted", 2)
	env := mustEnvelope(t, tenantID, submitted)
	// 同一事件投递两次：模拟 at-least-once 的重复。
	h.publish(t, env)
	h.publish(t, env)

	eventually(t, "事实行写入", func() (bool, error) {
		f, found, err := h.fact(tenantID, expenseID)
		if err != nil || !found {
			return found, err
		}
		assert.Equal(t, "submitted", f.Status)
		assert.Equal(t, "travel", f.Category)
		assert.Equal(t, "USD", f.Currency)
		assert.Equal(t, int64(12500), f.AmountMinor)
		assert.Equal(t, 2, f.Version)
		return true, nil
	})

	// 重复事件必须只留一条幂等记录。
	assert.Eventually(t, func() bool {
		n, err := h.processedCount(tenantID)
		return err == nil && n == 1
	}, 20*time.Second, 100*time.Millisecond, "同一事件应只记录一次")

	approved := snapshot(tenantID, expenseID, submitterID, "approved", 3)
	h.publish(t, mustEnvelope(t, tenantID, approved))

	eventually(t, "审批事件覆盖事实行", func() (bool, error) {
		f, found, err := h.fact(tenantID, expenseID)
		if err != nil || !found {
			return false, err
		}
		return f.Status == "approved" && f.Version == 3, nil
	})
	assert.Eventually(t, func() bool {
		n, err := h.processedCount(tenantID)
		return err == nil && n == 2
	}, 20*time.Second, 100*time.Millisecond)

	// 邻居租户看不到任何行（RLS + 事件过滤）。
	f, found, err := h.fact(uuid.New(), expenseID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Zero(t, f.Version)
}

func TestConsumerIgnoresOlderVersionsArrivingLate(t *testing.T) {
	h := newHarness(t)
	tenantID, expenseID, submitterID := uuid.New(), uuid.New(), uuid.New()

	// 先到 v3，再到 v2：旧事件不得把新状态回滚。
	h.publish(t, mustEnvelope(t, tenantID, snapshot(tenantID, expenseID, submitterID, "approved", 3)))
	eventually(t, "v3 写入", func() (bool, error) {
		f, found, err := h.fact(tenantID, expenseID)
		if err != nil || !found {
			return false, err
		}
		return f.Version == 3 && f.Status == "approved", nil
	})

	h.publish(t, mustEnvelope(t, tenantID, snapshot(tenantID, expenseID, submitterID, "submitted", 2)))
	assert.Eventually(t, func() bool {
		n, err := h.processedCount(tenantID)
		return err == nil && n == 2
	}, 20*time.Second, 100*time.Millisecond, "两个事件都应被消费")

	f, found, err := h.fact(tenantID, expenseID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 3, f.Version, "旧事件不能覆盖新版本")
	assert.Equal(t, "approved", f.Status)
}

func TestConsumerRoutesPermanentErrorsToDLQ(t *testing.T) {
	h := newHarness(t)
	tenantID, expenseID, submitterID := uuid.New(), uuid.New(), uuid.New()

	// 信封结构非法（event_id 不是 UUID）：重试永远不会成功。
	bad := []byte(`{"event_id":"not-a-uuid","event_type":"expense.submitted"}`)
	h.publishRaw(t, tenantID.String(), bad)

	dlqRecord := consumeOneDLQ(t, h)

	assert.Equal(t, bad, dlqRecord.Value, "DLQ 必须保留原始字节，便于直接重放")
	assert.Equal(t, tenantID.String(), string(dlqRecord.Key))
	headers := map[string]string{}
	for _, hd := range dlqRecord.Headers {
		headers[hd.Key] = string(hd.Value)
	}
	assert.Equal(t, h.topic, headers["x-original-topic"])
	assert.NotEmpty(t, headers["x-original-offset"])
	assert.Contains(t, headers["x-error"], "解码信封失败")

	// 坏消息被跳过并提交位点后，后续正常事件仍必须被处理。
	h.publish(t, mustEnvelope(t, tenantID, snapshot(tenantID, expenseID, submitterID, "submitted", 2)))
	eventually(t, "坏消息之后的有效事件", func() (bool, error) {
		f, found, err := h.fact(tenantID, expenseID)
		if err != nil || !found {
			return false, err
		}
		return f.Version == 2 && f.Status == "submitted", nil
	})
}

// consumeOneDLQ 用独立客户端从 DLQ 最早位点读取一条记录。
// 不加入消费组：避免与消费者自身的 rebalance 互相干扰。
func consumeOneDLQ(t *testing.T, h *harness) *kgo.Record {
	t.Helper()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(h.brokers...),
		kgo.ConsumeTopics(h.dlqTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	require.NoError(t, err)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		fetches := client.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatal("30 秒内未在 DLQ 收到消息")
		}
		if records := fetches.Records(); len(records) > 0 {
			return records[0]
		}
	}
}
