package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/outbox"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

var fixedTime = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

func newHarness(t *testing.T) (*db.DB, *testenv.Postgres) {
	t.Helper()
	pg := testenv.StartPostgres(t, "expense")
	database, err := db.Open(context.Background(), pg.AppDSN("expense"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	// outbox 对 app 角色是只追加的（DELETE 已被收回），清理只能用 migrator。
	conn, err := pgx.Connect(context.Background(), pg.MigratorDSN("expense"))
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `TRUNCATE app.outbox_events`)
	require.NoError(t, err)
	require.NoError(t, conn.Close(context.Background()))
	return database, pg
}

func newEnvelope(tenantID uuid.UUID) events.Envelope {
	snap := events.ExpenseSnapshot{
		ID:          uuid.New(),
		TenantID:    tenantID,
		SubmitterID: uuid.New(),
		Status:      "submitted",
		Category:    "travel",
		Currency:    "USD",
		AmountMinor: 12345,
		OccurredAt:  fixedTime.Add(-24 * time.Hour),
		Version:     2,
		CreatedAt:   fixedTime.Add(-time.Hour),
		UpdatedAt:   fixedTime,
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		panic(err)
	}
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             events.TypeExpenseSubmitted,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         tenantID,
		AggregateID:      snap.ID,
		AggregateVersion: 2,
		OccurredAt:       fixedTime,
		Payload:          raw,
	}
}

func insertEnvelope(t *testing.T, database *db.DB, store *outbox.Store, env events.Envelope) {
	t.Helper()
	require.NoError(t, database.WithinTenantID(context.Background(), env.TenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return store.Insert(ctx, tx, events.TopicExpense, env)
		}))
}

func claim(t *testing.T, database *db.DB, store *outbox.Store, limit int, lease time.Duration) []outbox.Event {
	t.Helper()
	var out []outbox.Event
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = store.Claim(ctx, tx, limit, lease)
		return err
	}))
	return out
}

func rowCount(t *testing.T, database *db.DB, where string) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.outbox_events `+where).Scan(&n)
	}))
	return n
}

func TestInsertAndClaim(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()
	tenantID := uuid.New()
	env := newEnvelope(tenantID)

	insertEnvelope(t, database, store, env)

	got := claim(t, database, store, 10, 30*time.Second)
	require.Len(t, got, 1)
	assert.Equal(t, env.EventID, got[0].ID)
	assert.Equal(t, tenantID, got[0].TenantID)
	assert.Equal(t, env.AggregateID, got[0].AggregateID)
	assert.Equal(t, env.AggregateVersion, got[0].AggregateVersion)
	assert.Equal(t, string(env.Type), got[0].Type)
	assert.Equal(t, events.TopicExpense, got[0].Topic)
	assert.Zero(t, got[0].Attempts)

	decoded, err := events.Decode(got[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, env.EventID, decoded.EventID)
	assert.Equal(t, env.TenantID, decoded.TenantID, "payload 里的租户必须与投递键一致")

	// 租约内不会被第二个投递器重复领取。
	assert.Empty(t, claim(t, database, store, 10, 30*time.Second))

	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.MarkPublished(ctx, tx, env.EventID)
	}))
	assert.Zero(t, rowCount(t, database, `WHERE published_at IS NULL`))

	// 已发布的事件不再出现，且行本身保留（审计）。
	assert.Empty(t, claim(t, database, store, 10, 30*time.Second))
	assert.Equal(t, 1, rowCount(t, database, `WHERE id = '`+env.EventID.String()+`'`))
}

func TestAppRoleCannotDeleteOutboxRows(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()
	env := newEnvelope(uuid.New())
	insertEnvelope(t, database, store, env)

	err := database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `DELETE FROM app.outbox_events WHERE id = $1`, env.EventID)
		return execErr
	})
	require.Error(t, err, "outbox 是只追加审计记录，运行时角色不能删除")
	assert.Contains(t, err.Error(), "permission denied")
	assert.Equal(t, 1, rowCount(t, database, ``))
}

func TestLeaseExpiresSoCrashedDispatcherDoesNotStrandEvents(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	insertEnvelope(t, database, store, newEnvelope(uuid.New()))
	require.Len(t, claim(t, database, store, 10, 200*time.Millisecond), 1)

	var got []outbox.Event
	require.Eventually(t, func() bool {
		got = claim(t, database, store, 10, 30*time.Second)
		return len(got) == 1
	}, 5*time.Second, 50*time.Millisecond, "租约到期后必须能重新领取")
	assert.Zero(t, got[0].Attempts, "崩溃没有走 MarkFailed，attempts 不变")
}

func TestMarkFailedRecordsRetry(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	insertEnvelope(t, database, store, newEnvelope(uuid.New()))
	got := claim(t, database, store, 10, time.Minute)
	require.Len(t, got, 1)

	longErr := errors.New(strings.Repeat("x", 600))
	retryAt := time.Now().Add(time.Hour)
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.MarkFailed(ctx, tx, got[0].ID, longErr, retryAt)
	}))

	// 还没到重试时间，不能领取。
	assert.Empty(t, claim(t, database, store, 10, time.Minute))

	var attempts int
	var lastError string
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT attempts, last_error FROM app.outbox_events WHERE id = $1`, got[0].ID).
			Scan(&attempts, &lastError)
	}))
	assert.Equal(t, 1, attempts)
	assert.Len(t, []rune(lastError), 480, "超长错误必须截断到列约束之内")

	// 把重试时间拨回过去，模拟退避到期。
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.outbox_events SET available_at = now() - interval '1 second' WHERE id = $1`,
			got[0].ID)
		return err
	}))
	again := claim(t, database, store, 10, time.Minute)
	require.Len(t, again, 1)
	assert.Equal(t, 1, again[0].Attempts, "重试时 attempts 应带出已失败次数")
}

func TestConcurrentClaimersDoNotBlockOrDuplicate(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	const total = 3
	for i := 0; i < total; i++ {
		insertEnvelope(t, database, store, newEnvelope(uuid.New()))
	}

	claimed := make(chan []outbox.Event, 1)
	failed := make(chan error, 1)
	release := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			first, err := store.Claim(ctx, tx, 1, time.Minute)
			if err != nil {
				return err
			}
			claimed <- first
			<-release // 持有事务与行锁，模拟"正在发送"的投递器
			return nil
		})
		if err != nil {
			failed <- err
		}
	}()

	var first []outbox.Event
	select {
	case first = <-claimed:
	case err := <-failed:
		t.Fatalf("第一个投递器领取失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("第一个投递器未能在超时内领取")
	}
	require.Len(t, first, 1)

	// 第二个投递器必须跳过被锁定的那一行，并且不被阻塞。
	second := claim(t, database, store, 10, time.Minute)
	require.Len(t, second, total-1)
	assert.NotEqual(t, first[0].ID, second[0].ID)

	release <- struct{}{}
	wg.Wait()

	assert.Equal(t, total, rowCount(t, database, `WHERE published_at IS NULL`))
}

func TestInsertIsPartOfCallerTransaction(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	boom := errors.New("business failure")
	tenantID := uuid.New()
	err := database.WithinTenantID(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := store.Insert(ctx, tx, events.TopicExpense, newEnvelope(tenantID)); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.Zero(t, rowCount(t, database, ``), "业务事务回滚时事件必须一起消失")
}

func TestInsertRejectsInvalidEnvelope(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	env := newEnvelope(uuid.New())
	env.SchemaVersion = 99
	err := database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.Insert(ctx, tx, events.TopicExpense, env)
	})
	require.Error(t, err)
	assert.Zero(t, rowCount(t, database, ``))
}

func TestClaimValidatesArguments(t *testing.T) {
	database, _ := newHarness(t)
	store := outbox.NewStore()

	require.Error(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Claim(ctx, tx, 0, time.Minute)
		return err
	}))
	require.Error(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.Claim(ctx, tx, 10, 0)
		return err
	}))
	require.Error(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return store.Insert(ctx, tx, "", newEnvelope(uuid.New()))
	}))
}
