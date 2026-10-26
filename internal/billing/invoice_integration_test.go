package billing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

type outboxRow struct {
	EventType string
	Topic     string
	Payload   []byte
}

func loadOutbox(t *testing.T, database *db.DB, ctx context.Context) []outboxRow {
	t.Helper()
	// outbox_events 没有 RLS（投递器要跨租户扫描），因此测试必须显式按租户过滤，
	// 否则同包其他用例产生的事件会混进来。
	var out []outboxRow
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT event_type, topic, payload FROM app.outbox_events
			 WHERE tenant_id = $1 ORDER BY created_at, event_type`, tenantctx.ID(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r outboxRow
			if err := rows.Scan(&r.EventType, &r.Topic, &r.Payload); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}))
	return out
}

func countEventType(rows []outboxRow, eventType string) int {
	n := 0
	for _, r := range rows {
		if r.EventType == eventType {
			n++
		}
	}
	return n
}

func TestBillingOutboxEvents(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }

	sub, err := svc.CreateSubscription(ctx, actor, SubscriptionInput{PlanCode: "growth-monthly", Seats: 5})
	require.NoError(t, err)

	rows := loadOutbox(t, database, ctx)
	require.Len(t, rows, 1)
	assert.Equal(t, "subscription.created", rows[0].EventType)
	assert.Equal(t, events.TopicBilling, rows[0].Topic)

	env, err := events.Decode(rows[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, events.TypeSubscriptionCreated, env.Type)
	assert.Equal(t, sub.ID, env.AggregateID)
	assert.Equal(t, sub.Version, env.AggregateVersion)
	var subSnap events.SubscriptionSnapshot
	require.NoError(t, json.Unmarshal(env.Payload, &subSnap))
	assert.Equal(t, tenantID, subSnap.TenantID)
	assert.Equal(t, "growth-monthly", subSnap.PlanCode)
	assert.Equal(t, 5, subSnap.Seats)
	assert.Equal(t, "active", subSnap.Status)

	inv, err := svc.GenerateInvoice(ctx, actor, sub.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 49500, inv.AmountMinor)
	assert.True(t, strings.HasPrefix(inv.Number, "INV-202610-"), inv.Number)
	assert.Len(t, inv.Number, len("INV-202610-")+8)
	assert.Equal(t, "2026-11-09", dateString(inv.DueOn))

	rows = loadOutbox(t, database, ctx)
	require.Len(t, rows, 2)
	assert.Equal(t, 1, countEventType(rows, "invoice.created"))
	for _, r := range rows {
		if r.EventType != "invoice.created" {
			continue
		}
		env, err := events.Decode(r.Payload)
		require.NoError(t, err)
		var snap events.InvoiceSnapshot
		require.NoError(t, json.Unmarshal(env.Payload, &snap))
		assert.Equal(t, inv.ID, snap.ID)
		assert.EqualValues(t, 49500, snap.AmountMinor)
		assert.Equal(t, "open", snap.Status)
		assert.Equal(t, "USD", snap.Currency)
	}

	// 重复出账被唯一约束拒绝，事务回滚后 outbox 里不能多出事件。
	_, err = svc.GenerateInvoice(ctx, actor, sub.ID)
	require.Error(t, err)
	e, ok := apperr.As(err)
	require.True(t, ok)
	assert.Equal(t, CodeInvoiceExists, e.Code())
	assert.Len(t, loadOutbox(t, database, ctx), 2)

	canceled, err := svc.CancelSubscription(ctx, actor, sub.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, SubscriptionCanceled, canceled.Status)

	rows = loadOutbox(t, database, ctx)
	require.Len(t, rows, 3)
	assert.Equal(t, 1, countEventType(rows, "subscription.canceled"))
	for _, r := range rows {
		if r.EventType != "subscription.canceled" {
			continue
		}
		env, err := events.Decode(r.Payload)
		require.NoError(t, err)
		var snap events.SubscriptionSnapshot
		require.NoError(t, json.Unmarshal(env.Payload, &snap))
		assert.Equal(t, "canceled", snap.Status)
		require.NotNil(t, snap.CanceledAt)
	}

	// 已取消的订阅不能出账。
	_, err = svc.GenerateInvoice(ctx, actor, sub.ID)
	require.Error(t, err)
	e, ok = apperr.As(err)
	require.True(t, ok)
	assert.Equal(t, CodeSubscriptionStateConflict, e.Code())
	assert.Len(t, loadOutbox(t, database, ctx), 3)
}
