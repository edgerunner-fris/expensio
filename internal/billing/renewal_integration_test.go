package billing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// insertSubscriptionWithPeriod 直接写库构造一条指定周期与状态的订阅，
// 用于把"已到期"的起点固定在测试里，而不是等待真实时间。
func insertSubscriptionWithPeriod(t *testing.T, database *db.DB, ctx context.Context,
	plan string, seats int, start, end, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tenant := mustTenant(t, ctx)
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var canceledAt any
		if status == string(SubscriptionCanceled) {
			canceledAt = time.Now().UTC()
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats, status,
			     current_period_start, current_period_end, canceled_at,
			     created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
			id, tenant.ID, plan, seats, status, start, end, canceledAt, tenant.UserID)
		return err
	}))
	return id
}

func payInvoice(t *testing.T, database *db.DB, ctx context.Context, invoiceID uuid.UUID) {
	t.Helper()
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.invoices SET status = 'paid', paid_at = now() WHERE id = $1`, invoiceID)
		return err
	}))
}

func dateStringOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

func TestRenewalRenewsDueSubscriptionOnce(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }

	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"growth-monthly", 3, "2026-09-01", "2026-09-30", "active")

	report, err := svc.RunRenewal(ctx, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, report.Renewed, 1)

	sub, err := svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-01", dateStringOf(sub.CurrentPeriodStart))
	assert.Equal(t, "2026-10-31", dateStringOf(sub.CurrentPeriodEnd))
	// 首次周期从 9 月 1 日开始，锚点日就是 1。
	assert.Equal(t, 1, sub.AnchorDay)
	assert.Equal(t, 2, sub.Version)

	invoices, total, err := svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	inv := invoices[0]
	assert.Equal(t, int64(9900*3), inv.AmountMinor)
	assert.Equal(t, "2026-10-01", dateStringOf(inv.PeriodStart))
	assert.Equal(t, "2026-10-31", dateStringOf(inv.PeriodEnd))
	assert.Equal(t, InvoiceOpen, inv.Status)
	// 到期日 = 出账日 + 14 天（billingTestNow = 2026-10-26）。
	assert.Equal(t, "2026-11-09", dateStringOf(inv.DueOn))

	rows := loadOutbox(t, database, ctx)
	assert.Equal(t, 1, countEventType(rows, "invoice.created"))

	// 再跑一轮：周期已经前移，不能重复出账，也不能再改版本。
	_, err = svc.RunRenewal(ctx, 100)
	require.NoError(t, err)

	sub, err = svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-10-31", dateStringOf(sub.CurrentPeriodEnd))
	assert.Equal(t, 2, sub.Version)

	_, total, err = svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
}

func TestRenewalSkipsSubscriptionWithUnpaidInvoice(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }

	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"growth-monthly", 2, "2026-08-01", "2026-08-31", "active")
	insertInvoice(t, database, ctx, subID, 19800)

	report, err := svc.RunRenewal(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.Zero(t, report.Renewed)

	sub, err := svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-08-31", dateStringOf(sub.CurrentPeriodEnd))
	assert.Equal(t, 1, sub.Version)

	_, total, err := svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "欠费时不能为下一周期再出账")
}

func TestRenewalIgnoresCanceledSubscription(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }

	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"starter-monthly", 1, "2026-07-01", "2026-07-31", "canceled")

	_, err := svc.RunRenewal(ctx, 100)
	require.NoError(t, err)

	sub, err := svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-07-31", dateStringOf(sub.CurrentPeriodEnd))
	assert.Equal(t, 1, sub.Version)

	_, total, err := svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	assert.Zero(t, total)
}

// 1 月 31 日开通的月付订阅：2 月周期结束于 27 日，3 月必须回到 31 日，
// 而不是沿着 2 月的 28 日一路漂移。这是 anchor_day 存在的唯一理由。
func TestRenewalKeepsAnchorDayAcrossShortMonths(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"growth-monthly", 1, "2026-01-31", "2026-02-27", "active")

	svc.now = func() time.Time { return utcDate(2026, 2, 28) }
	_, err := svc.RunRenewal(ctx, 100)
	require.NoError(t, err)

	sub, err := svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-28", dateStringOf(sub.CurrentPeriodStart))
	assert.Equal(t, "2026-03-30", dateStringOf(sub.CurrentPeriodEnd))
	assert.Equal(t, 31, sub.AnchorDay)

	invoices, total, err := svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	payInvoice(t, database, ctx, invoices[0].ID)

	svc.now = func() time.Time { return utcDate(2026, 3, 31) }
	_, err = svc.RunRenewal(ctx, 100)
	require.NoError(t, err)

	sub, err = svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-03-31", dateStringOf(sub.CurrentPeriodStart))
	assert.Equal(t, "2026-04-29", dateStringOf(sub.CurrentPeriodEnd))

	_, total, err = svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
}

func TestRenewalSkipsPastDueSubscription(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	actor := mustTenant(t, ctx)

	svc := NewService(database)
	svc.now = func() time.Time { return billingTestNow }

	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"growth-monthly", 1, "2026-09-01", "2026-09-30", "past_due")

	_, err := svc.RunRenewal(ctx, 100)
	require.NoError(t, err)

	sub, err := svc.GetSubscription(ctx, actor, subID)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-30", dateStringOf(sub.CurrentPeriodEnd))
	assert.Equal(t, 1, sub.Version)

	_, total, err := svc.ListInvoices(ctx, actor, InvoiceQuery{Limit: 20})
	require.NoError(t, err)
	assert.Zero(t, total)
}

func TestPeriodGuardOnlyAllowsContiguousRollover(t *testing.T) {
	database, _ := newDB(t)
	ctx := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleOwner)

	subID := insertSubscriptionWithPeriod(t, database, ctx,
		"growth-monthly", 1, "2026-09-01", "2026-09-30", "active")

	update := func(set string) error {
		return database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE app.subscriptions SET `+set+` WHERE id = $1`, subID)
			return err
		})
	}

	// 倒退。
	requireSQLState(t, "23514", update(
		`current_period_start = '2026-08-01', current_period_end = '2026-08-31'`))
	// 跳空：新开始日不是旧结束日的次日。
	requireSQLState(t, "23514", update(
		`current_period_start = '2026-10-02', current_period_end = '2026-10-31'`))
	// 只改结束日。
	requireSQLState(t, "23514", update(`current_period_end = '2026-10-31'`))
	// 周期滚动不能顺带改状态。
	requireSQLState(t, "23514", update(
		`current_period_start = '2026-10-01', current_period_end = '2026-10-31', status = 'past_due'`))
	// 锚点日不可变。
	requireSQLState(t, "23514", update(`anchor_day = 15`))

	// 连续向前滚动一个周期是唯一被允许的形状。
	require.NoError(t, update(
		`current_period_start = '2026-10-01', current_period_end = '2026-10-31'`))
}
