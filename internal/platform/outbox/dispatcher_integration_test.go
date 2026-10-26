package outbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/outbox"
)

type published struct {
	topic string
	key   string
	value []byte
}

type fakePublisher struct {
	mu       sync.Mutex
	failNext int
	cause    error
	records  []published
}

func (f *fakePublisher) Publish(_ context.Context, topic, key string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return f.cause
	}
	f.records = append(f.records, published{
		topic: topic,
		key:   key,
		value: append([]byte(nil), value...),
	})
	return nil
}

func (f *fakePublisher) snapshot() []published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]published(nil), f.records...)
}

func TestDispatcherPublishesClaimedEvents(t *testing.T) {
	database, _ := newHarness(t)
	pub := &fakePublisher{}
	d, err := outbox.NewDispatcher(outbox.DispatcherOptions{
		Database:       database,
		Publisher:      pub,
		BatchSize:      10,
		PublishTimeout: time.Second,
	})
	require.NoError(t, err)

	first := newEnvelope(uuid.New())
	second := newEnvelope(uuid.New())
	insertEnvelope(t, database, outbox.NewStore(), first)
	insertEnvelope(t, database, outbox.NewStore(), second)

	n, err := d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Zero(t, rowCount(t, database, `WHERE published_at IS NULL`))
	assert.Equal(t, 2, rowCount(t, database, `WHERE published_at IS NOT NULL`))

	records := pub.snapshot()
	require.Len(t, records, 2)
	byTenant := map[string]published{}
	for _, r := range records {
		byTenant[r.key] = r
	}
	for _, env := range []events.Envelope{first, second} {
		r, ok := byTenant[env.TenantID.String()]
		require.True(t, ok, "分区键必须是 tenant_id")
		assert.Equal(t, events.TopicExpense, r.topic)
		raw, err := env.Encode()
		require.NoError(t, err)
		assert.JSONEq(t, string(raw), string(r.value), "发送的必须是完整信封")
	}

	n, err = d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "已发布事件不应被再次领取")
}

func TestDispatcherRetriesFailedPublish(t *testing.T) {
	database, _ := newHarness(t)
	pub := &fakePublisher{failNext: 1, cause: errors.New("broker unavailable")}
	d, err := outbox.NewDispatcher(outbox.DispatcherOptions{
		Database:       database,
		Publisher:      pub,
		BatchSize:      10,
		PublishTimeout: time.Second,
	})
	require.NoError(t, err)

	env := newEnvelope(uuid.New())
	insertEnvelope(t, database, outbox.NewStore(), env)

	n, err := d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, pub.snapshot())
	assert.Equal(t, 1, rowCount(t, database, `WHERE published_at IS NULL AND attempts = 1 AND last_error <> ''`))

	// 退避未到期：不能立即重试，否则失败风暴会打满 broker。
	n, err = d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)

	// 模拟退避到期。
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.outbox_events SET available_at = now() - interval '1 second' WHERE id = $1`,
			env.EventID)
		return err
	}))

	n, err = d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, pub.snapshot(), 1)
	assert.Zero(t, rowCount(t, database, `WHERE published_at IS NULL`))
}

func TestDispatcherContinuesBatchAfterOneFailure(t *testing.T) {
	database, _ := newHarness(t)
	pub := &fakePublisher{failNext: 1, cause: errors.New("transient")}
	d, err := outbox.NewDispatcher(outbox.DispatcherOptions{
		Database:       database,
		Publisher:      pub,
		BatchSize:      10,
		PublishTimeout: time.Second,
	})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		insertEnvelope(t, database, outbox.NewStore(), newEnvelope(uuid.New()))
	}

	n, err := d.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Len(t, pub.snapshot(), 2, "一条失败不能阻塞同批其他事件")
	assert.Equal(t, 1, rowCount(t, database, `WHERE published_at IS NULL AND attempts = 1`))
	assert.Equal(t, 2, rowCount(t, database, `WHERE published_at IS NOT NULL`))
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{
		0:  time.Second,
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		6:  32 * time.Second,
		7:  time.Minute,
		20: time.Minute,
	}
	for attempts, want := range cases {
		assert.Equal(t, want, outbox.Backoff(attempts), "attempts=%d", attempts)
	}
}

func TestNewDispatcherRequiresDependencies(t *testing.T) {
	_, err := outbox.NewDispatcher(outbox.DispatcherOptions{Publisher: &fakePublisher{}})
	require.Error(t, err)
	_, err = outbox.NewDispatcher(outbox.DispatcherOptions{Database: nil, Publisher: nil})
	require.Error(t, err)
}
