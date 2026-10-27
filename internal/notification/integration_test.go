package notification

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

// newNotificationDB 返回一个已清空的 tenant 数据环境。
//
// 集成测试共享同一个 PostgreSQL 实例，用属主连接 TRUNCATE 做隔离：
// TRUNCATE 是表级操作，不受 RLS 策略影响；运行时账号做不到这件事，
// 也不应该做得到。
func newNotificationDB(t *testing.T) *db.DB {
	t.Helper()
	pg := testenv.StartPostgres(t, "notification")
	database, err := db.Open(context.Background(), pg.AppDSN("notification"), 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pg.MigratorDSN("notification"))
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `TRUNCATE app.email_outbox, app.notifications, app.processed_events`)
	require.NoError(t, err)
	require.NoError(t, conn.Close(ctx))
	return database
}

type stubContacts struct {
	mu       sync.Mutex
	byTenant map[uuid.UUID][]Contact
	err      error
	calls    []uuid.UUID
}

func (s *stubContacts) TenantContacts(_ context.Context, tenantID uuid.UUID) ([]Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, tenantID)
	if s.err != nil {
		return nil, s.err
	}
	return s.byTenant[tenantID], nil
}

func newTestConsumer(database *db.DB, contacts ContactsSource, email bool) *Consumer {
	return &Consumer{
		database:     database,
		contacts:     contacts,
		repo:         NewRepository(),
		logger:       zap.NewNop(),
		consumerName: events.ConsumerNotificationExpense,
		emailEnabled: email,
		kind:         topicExpense,
	}
}

func countNotifications(t *testing.T, database *db.DB, tenantID, userID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM app.notifications WHERE user_id = $1`, userID).Scan(&n)
		}))
	return n
}

func countProcessed(t *testing.T, database *db.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.processed_events`).Scan(&n)
		}))
	return n
}

func countEmails(t *testing.T, database *db.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.email_outbox`).Scan(&n)
		}))
	return n
}

func TestExpenseConsumerIsIdempotentAndTenantScoped(t *testing.T) {
	database := newNotificationDB(t)
	env := expenseEnvelope(t, events.TypeExpenseSubmitted, "submitted", 1, 10000)
	snap, err := decodeExpenseEvent(env)
	require.NoError(t, err)

	owner := Contact{UserID: uuid.New(), Email: "owner@example.com", DisplayName: "Owner", Role: "owner"}
	submitter := Contact{UserID: snap.SubmitterID, Email: "admin@example.com", DisplayName: "Admin", Role: "admin"}
	member := Contact{UserID: uuid.New(), Email: "member@example.com", DisplayName: "Member", Role: "member"}

	contacts := &stubContacts{byTenant: map[uuid.UUID][]Contact{
		env.TenantID: {owner, submitter, member},
	}}
	consumer := newTestConsumer(database, contacts, true)
	msg := kafka.Message{Value: mustEncode(t, env)}

	require.NoError(t, consumer.Handle(context.Background(), msg))
	assert.Equal(t, 1, countNotifications(t, database, env.TenantID, owner.UserID))
	assert.Zero(t, countNotifications(t, database, env.TenantID, submitter.UserID),
		"提交者自己不应收到待审批提醒")
	assert.Zero(t, countNotifications(t, database, env.TenantID, member.UserID))
	assert.Equal(t, 1, countEmails(t, database, env.TenantID))
	assert.Equal(t, 1, countProcessed(t, database, env.TenantID))

	// 重放同一条事件：幂等键吸收，通知与邮件都不增加。
	require.NoError(t, consumer.Handle(context.Background(), msg))
	assert.Equal(t, 1, countNotifications(t, database, env.TenantID, owner.UserID))
	assert.Equal(t, 1, countEmails(t, database, env.TenantID))
	assert.Equal(t, 1, countProcessed(t, database, env.TenantID))

	// 另一个租户看不到任何行，即使知道 event_id 也读不到。
	otherTenant := uuid.New()
	assert.Zero(t, countNotifications(t, database, otherTenant, owner.UserID))
	assert.Zero(t, countEmails(t, database, otherTenant))

	// 没有租户上下文时 RLS 让整表不可见。
	var total int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.notifications`).Scan(&total)
	}))
	assert.Zero(t, total)
}

func TestConsumerRetriesWhenContactsUnavailable(t *testing.T) {
	database := newNotificationDB(t)
	env := expenseEnvelope(t, events.TypeExpenseApproved, "approved", 2, 500)
	contacts := &stubContacts{err: errors.New("identity down")}
	consumer := newTestConsumer(database, contacts, true)

	err := consumer.Handle(context.Background(), kafka.Message{Value: mustEncode(t, env)})
	require.Error(t, err)
	assert.Equal(t, 0, countProcessed(t, database, env.TenantID),
		"取收件人失败时不能提交幂等键，否则事件会被永久丢掉")
}

func TestConsumerSendsContractViolationsToDLQ(t *testing.T) {
	database := newNotificationDB(t)
	contacts := &stubContacts{}
	consumer := newTestConsumer(database, contacts, true)

	env := expenseEnvelope(t, events.TypeExpenseApproved, "submitted", 2, 500)
	err := consumer.Handle(context.Background(), kafka.Message{Value: mustEncode(t, env)})
	require.Error(t, err)
	assert.True(t, kafka.IsPermanent(err), "契约冲突必须走 DLQ，而不是无限重试")
	assert.Empty(t, contacts.calls, "坏消息不应该触发外部请求")
}

func TestServiceReadsOnlyOwnInboxAndMarksRead(t *testing.T) {
	database := newNotificationDB(t)
	tenantID := uuid.New()
	ownerID, memberID := uuid.New(), uuid.New()

	insert := func(userID uuid.UUID) uuid.UUID {
		id := uuid.New()
		require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO app.notifications
					    (id, tenant_id, user_id, kind, title, body, event_id)
					VALUES ($1, $2, $3, 'expense.submitted', '待审批', '测试通知', $4)`,
					id, tenantID, userID, uuid.New())
				return err
			}))
		return id
	}
	ownerOne := insert(ownerID)
	insert(ownerID)
	memberOne := insert(memberID)

	svc := NewService(database)
	ownerCtx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID: tenantID, UserID: ownerID, Role: tenantctx.RoleOwner,
	})
	require.NoError(t, err)
	memberCtx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID: tenantID, UserID: memberID, Role: tenantctx.RoleMember,
	})
	require.NoError(t, err)

	page, err := svc.List(ownerCtx, listQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), page.Total)
	assert.Len(t, page.Items, 2)

	count, err := svc.UnreadCount(ownerCtx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)

	// 成员只能看到自己的那一条。
	page, err = svc.List(memberCtx, listQuery{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), page.Total)
	assert.Equal(t, memberOne, page.Items[0].ID)

	// 不能标记别人的通知。
	err = svc.MarkRead(memberCtx, ownerOne)
	require.Error(t, err)
	assert.Equal(t, apperr.CodeNotFound, apperr.Code(err))

	require.NoError(t, svc.MarkRead(ownerCtx, ownerOne))
	count, err = svc.UnreadCount(ownerCtx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	updated, err := svc.MarkAllRead(ownerCtx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated)
	count, err = svc.UnreadCount(ownerCtx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

type fakeMailer struct {
	mu   sync.Mutex
	sent []string
	fail error
}

func (f *fakeMailer) Send(_ context.Context, to, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, to)
	return nil
}

func insertEmail(t *testing.T, database *db.DB, tenantID uuid.UUID, dedupe string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.email_outbox (id, tenant_id, recipient, subject, body, dedupe_key)
				VALUES ($1, $2, 'user@example.com', '主题', '正文', $3)`,
				id, tenantID, dedupe)
			return err
		}))
	return id
}

func emailStatus(t *testing.T, database *db.DB, tenantID, id uuid.UUID) (string, int, *time.Time) {
	t.Helper()
	var status string
	var attempts int
	var sentAt *time.Time
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT status, attempts, sent_at FROM app.email_outbox WHERE id = $1`, id).
				Scan(&status, &attempts, &sentAt)
		}))
	return status, attempts, sentAt
}

func TestEmailWorkerClaimsAcrossTenantsAndMarksSent(t *testing.T) {
	database := newNotificationDB(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	mailA := insertEmail(t, database, tenantA, "a")
	mailB := insertEmail(t, database, tenantB, "b")

	mailer := &fakeMailer{}
	worker, err := NewEmailWorker(EmailWorkerOptions{
		Database: database, Mailer: mailer, Logger: zap.NewNop(), Interval: time.Second, Batch: 10,
	})
	require.NoError(t, err)

	processed, err := worker.ProcessOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	assert.ElementsMatch(t, []string{"user@example.com", "user@example.com"}, mailer.sent)

	for _, tc := range []struct {
		tenant uuid.UUID
		id     uuid.UUID
	}{{tenantA, mailA}, {tenantB, mailB}} {
		status, attempts, sentAt := emailStatus(t, database, tc.tenant, tc.id)
		assert.Equal(t, "sent", status)
		assert.Equal(t, 1, attempts)
		assert.NotNil(t, sentAt)
	}
}

func TestEmailWorkerSchedulesRetryAndStopsAtAttemptLimit(t *testing.T) {
	database := newNotificationDB(t)
	tenantID := uuid.New()
	mailID := insertEmail(t, database, tenantID, "retry")

	worker, err := NewEmailWorker(EmailWorkerOptions{
		Database: database, Mailer: &fakeMailer{fail: errors.New("smtp down")},
		Logger: zap.NewNop(), Interval: time.Second, Batch: 10,
	})
	require.NoError(t, err)

	processed, err := worker.ProcessOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)

	status, attempts, sentAt := emailStatus(t, database, tenantID, mailID)
	assert.Equal(t, "failed", status)
	assert.Equal(t, 1, attempts)
	assert.Nil(t, sentAt)

	// 模拟已经重试到上限：claim_emails 不再领取，避免无限重试。
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE app.email_outbox
				SET attempts = 5, status = 'failed', next_attempt_at = now() - interval '1 second'
				WHERE id = $1`, mailID)
			return err
		}))

	mailer := &fakeMailer{}
	worker, err = NewEmailWorker(EmailWorkerOptions{
		Database: database, Mailer: mailer, Logger: zap.NewNop(), Interval: time.Second, Batch: 10,
	})
	require.NoError(t, err)
	processed, err = worker.ProcessOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, processed)
	assert.Empty(t, mailer.sent)
}
