package billing

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestBillingSchemaApplies(t *testing.T) {
	database, _ := newDB(t)

	var plans int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.plans`).Scan(&plans)
	}))
	assert.Equal(t, 5, plans, "迁移必须种入套餐目录")

	var forced int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'app'
			  AND c.relname IN ('subscriptions', 'invoices', 'payments', 'webhook_events')
			  AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&forced)
	}))
	assert.Equal(t, 4, forced, "四张租户表都必须 ENABLE + FORCE RLS")

	privileges := []struct {
		name string
		sql  string
		want bool
	}{
		{"套餐不可写", `SELECT has_table_privilege('billing_app', 'app.plans', 'INSERT')`, false},
		{"账单不可删", `SELECT has_table_privilege('billing_app', 'app.invoices', 'DELETE')`, false},
		{"支付流水不可改", `SELECT has_table_privilege('billing_app', 'app.payments', 'UPDATE')`, false},
		{"回调记录不可删", `SELECT has_table_privilege('billing_app', 'app.webhook_events', 'DELETE')`, false},
		{"映射表不可直读", `SELECT has_table_privilege('billing_app', 'app.invoice_lookup', 'SELECT')`, false},
		{"租户反查函数可执行", `SELECT has_function_privilege('billing_app', 'app.invoice_tenant(uuid)', 'EXECUTE')`, true},
	}
	for _, p := range privileges {
		t.Run(p.name, func(t *testing.T) {
			var got bool
			require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, p.sql).Scan(&got)
			}))
			assert.Equal(t, p.want, got)
		})
	}
}

func TestBillingRLSIsolatesTenants(t *testing.T) {
	database, _ := newDB(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	userA, userB := uuid.New(), uuid.New()
	ctxA := tenantCtx(t, tenantA, userA, tenantctx.RoleOwner)
	ctxB := tenantCtx(t, tenantB, userB, tenantctx.RoleOwner)

	subA := insertSubscription(t, database, ctxA, "starter-monthly", 3)
	invoiceA := insertInvoice(t, database, ctxA, subA, 8700)

	assert.Equal(t, 1, countRows(t, database, ctxA, `SELECT count(*) FROM app.subscriptions`))
	assert.Equal(t, 0, countRows(t, database, ctxB, `SELECT count(*) FROM app.subscriptions`))
	assert.Equal(t, 1, countRows(t, database, ctxA, `SELECT count(*) FROM app.invoices`))
	assert.Equal(t, 0, countRows(t, database, ctxB, `SELECT count(*) FROM app.invoices`))

	// 没有租户上下文时，RLS 策略不通过，查不到任何行。
	assert.Equal(t, 0, countWithoutTenant(t, database, `SELECT count(*) FROM app.subscriptions`))
	assert.Equal(t, 0, countWithoutTenant(t, database, `SELECT count(*) FROM app.invoices`))

	// 租户 A 的上下文里写租户 B 的行必须被 WITH CHECK 拒绝。
	err := database.WithinTenant(ctxA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats, current_period_start, current_period_end,
			     created_by, updated_by)
			VALUES ($1, $2, 'starter-monthly', 1, '2026-10-01', '2026-10-31', $3, $3)`,
			uuid.New(), tenantB, userA)
		return err
	})
	requireSQLState(t, "42501", err)

	// 跨租户按 ID 更新同样命中 0 行。
	var affected int64
	require.NoError(t, database.WithinTenant(ctxB, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE app.invoices SET status = status WHERE id = $1`, invoiceA)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	}))
	assert.Zero(t, affected)
}

func TestSubscriptionConstraints(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)

	subID := insertSubscription(t, database, ctx, "growth-monthly", 5)

	// 同一租户第二条未取消订阅必须失败。
	err := database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats, current_period_start, current_period_end,
			     created_by, updated_by)
			VALUES ($1, $2, 'starter-monthly', 1, '2026-10-01', '2026-10-31', $3, $3)`,
			uuid.New(), tenantID, userID)
		return err
	})
	requireSQLState(t, "23505", err)

	// 席位与套餐的边界由数据库拦。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats, current_period_start, current_period_end,
			     created_by, updated_by)
			VALUES ($1, $2, 'starter-monthly', 0, '2026-10-01', '2026-10-31', $3, $3)`,
			uuid.New(), tenantID, userID)
		return err
	})
	requireSQLState(t, "23514", err)

	// 套餐外键要在另一个租户里验证：同一租户会先撞上"只能有一条未取消订阅"。
	ctxOther := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleOwner)
	err = database.WithinTenant(ctxOther, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.subscriptions
			    (id, tenant_id, plan_code, seats, current_period_start, current_period_end,
			     created_by, updated_by)
			VALUES ($1, $2, 'no-such-plan', 1, '2026-10-01', '2026-10-31', $3, $3)`,
			uuid.New(), mustTenant(t, ctxOther).ID, mustTenant(t, ctxOther).UserID)
		return err
	})
	requireSQLState(t, "23503", err)

	// 套餐与周期在创建后不可变。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.subscriptions SET plan_code = 'scale-monthly' WHERE id = $1`, subID)
		return err
	})
	requireSQLState(t, "23514", err)

	// active → past_due → active 合法，且版本每次自增。
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.subscriptions SET status = 'past_due' WHERE id = $1`, subID)
		return err
	}))
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.subscriptions SET status = 'active' WHERE id = $1`, subID)
		return err
	}))

	var version int
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT version FROM app.subscriptions WHERE id = $1`, subID).Scan(&version)
	}))
	assert.Equal(t, 3, version)

	// 取消必须同时写 canceled_at；只改状态会被约束拒绝。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.subscriptions SET status = 'canceled' WHERE id = $1`, subID)
		return err
	})
	requireSQLState(t, "23514", err)

	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, subID)
		return err
	}))

	// canceled 是终态。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.subscriptions SET status = 'active', canceled_at = NULL WHERE id = $1`, subID)
		return err
	})
	requireSQLState(t, "23514", err)

	// 取消后可以重新订阅。
	insertSubscription(t, database, ctx, "starter-monthly", 1)
}

func TestInvoiceConstraintsAndLookup(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)

	subID := insertSubscription(t, database, ctx, "growth-monthly", 2)
	invoiceID := insertInvoice(t, database, ctx, subID, 19800)

	// 同一订阅周期重复出账必须失败。
	err := database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.invoices
			    (id, tenant_id, subscription_id, number,
			     period_start, period_end, currency, amount_minor, due_on)
			VALUES ($1, $2, $3, $4, '2026-10-01', '2026-10-31', 'USD', 19800, '2026-10-15')`,
			uuid.New(), tenantID, subID, "INV-DUP-"+uuid.New().String()[:8])
		return err
	})
	requireSQLState(t, "23505", err)

	// 金额、币种、周期出账后不可改。
	for name, sql := range map[string]string{
		"金额": `UPDATE app.invoices SET amount_minor = amount_minor + 1 WHERE id = $1`,
		"币种": `UPDATE app.invoices SET currency = 'EUR' WHERE id = $1`,
		"周期": `UPDATE app.invoices SET period_end = '2026-11-30' WHERE id = $1`,
	} {
		t.Run(name+"不可改", func(t *testing.T) {
			err := database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, invoiceID)
				return err
			})
			requireSQLState(t, "23514", err)
		})
	}

	// 标记已付但没有付款时间会被 CHECK 拒绝。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.invoices SET status = 'paid' WHERE id = $1`, invoiceID)
		return err
	})
	requireSQLState(t, "23514", err)

	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.invoices SET status = 'paid', paid_at = now() WHERE id = $1`, invoiceID)
		return err
	}))

	var version int
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT version FROM app.invoices WHERE id = $1`, invoiceID).Scan(&version)
	}))
	assert.Equal(t, 2, version)

	// paid 是终态，不能回到 open。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.invoices SET status = 'open', paid_at = NULL WHERE id = $1`, invoiceID)
		return err
	})
	requireSQLState(t, "23514", err)

	// 非法状态在插入时就被拒绝。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.invoices
			    (id, tenant_id, subscription_id, number, status,
			     period_start, period_end, currency, amount_minor, due_on)
			VALUES ($1, $2, $3, $4, 'draft', '2026-11-01', '2026-11-30', 'USD', 100, '2026-11-15')`,
			uuid.New(), tenantID, subID, "INV-DRAFT-"+uuid.New().String()[:8])
		return err
	})
	requireSQLState(t, "23514", err)

	// 回调映射由触发器在同事务写入，运行时角色只能通过函数反查。
	var resolved uuid.UUID
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT app.invoice_tenant($1)`, invoiceID).Scan(&resolved)
	}))
	assert.Equal(t, tenantID, resolved)

	err = database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.invoice_lookup`).Scan(&n)
	})
	requireSQLState(t, "42501", err)

	var unknown pgtype.UUID
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT app.invoice_tenant($1)`, uuid.New()).Scan(&unknown)
	}))
	assert.False(t, unknown.Valid, "未知账单不能凭空构造出租户")
}

func TestPaymentsWebhooksAndOutbox(t *testing.T) {
	database, _ := newDB(t)
	tenantID, userID := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenantID, userID, tenantctx.RoleOwner)
	subID := insertSubscription(t, database, ctx, "growth-monthly", 2)
	invoiceID := insertInvoice(t, database, ctx, subID, 19800)

	insertPayment := func(paymentID string) error {
		return database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.payments
				    (id, tenant_id, invoice_id, provider, provider_payment_id,
				     amount_minor, currency, paid_at)
				VALUES ($1, $2, $3, 'stripe', $4, 19800, 'USD', now())`,
				uuid.New(), tenantID, invoiceID, paymentID)
			return err
		})
	}
	require.NoError(t, insertPayment("pay_123"))
	requireSQLState(t, "23505", insertPayment("pay_123"))

	// 支付流水只追加。
	err := database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.payments SET amount_minor = 1`)
		return err
	})
	requireSQLState(t, "42501", err)
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM app.payments`)
		return err
	})
	requireSQLState(t, "42501", err)

	insertWebhook := func(eventID string) error {
		return database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.webhook_events
				    (id, tenant_id, provider, provider_event_id, event_type,
				     invoice_id, payload_sha256)
				VALUES ($1, $2, 'stripe', $3, 'payment.succeeded', $4, $5)`,
				uuid.New(), tenantID, eventID, invoiceID, make([]byte, 32))
			return err
		})
	}
	require.NoError(t, insertWebhook("evt_1"))
	requireSQLState(t, "23505", insertWebhook("evt_1"))

	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM app.webhook_events`)
		return err
	})
	requireSQLState(t, "42501", err)

	// 回调事件类型白名单。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.webhook_events
			    (id, tenant_id, provider, provider_event_id, event_type,
			     invoice_id, payload_sha256)
			VALUES ($1, $2, 'stripe', 'evt_bad', 'charge.refunded', $3, $4)`,
			uuid.New(), tenantID, invoiceID, make([]byte, 32))
		return err
	})
	requireSQLState(t, "23514", err)

	// outbox 的事件类型同样受约束，且不可删除。
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.outbox_events
			    (id, tenant_id, aggregate_id, aggregate_version, event_type, topic, payload)
			VALUES ($1, $2, $3, 1, 'expense.approved', 'expensio.billing.events.v1', '{}'::jsonb)`,
			uuid.New(), tenantID, invoiceID)
		return err
	})
	requireSQLState(t, "23514", err)

	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.outbox_events
			    (id, tenant_id, aggregate_id, aggregate_version, event_type, topic, payload)
			VALUES ($1, $2, $3, 1, 'invoice.created', 'expensio.billing.events.v1', '{}'::jsonb)`,
			uuid.New(), tenantID, invoiceID)
		return err
	}))
	err = database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM app.outbox_events`)
		return err
	})
	requireSQLState(t, "42501", err)

	// 其它租户看不到支付流水与回调记录。
	ctxOther := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleOwner)
	assert.Zero(t, countRows(t, database, ctxOther, `SELECT count(*) FROM app.payments`))
	assert.Zero(t, countRows(t, database, ctxOther, `SELECT count(*) FROM app.webhook_events`))
}

func TestBillingMigrationDownAndUp(t *testing.T) {
	database, pg := newDB(t)
	ctx := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleOwner)
	insertSubscription(t, database, ctx, "starter-monthly", 1)

	pg.MigrateDownUp(t, "billing", 1)

	var plans int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.plans`).Scan(&plans)
	}))
	assert.Equal(t, 5, plans, "down/up 之后套餐种子仍在")

	// 表被重建：原来的订阅行不存在。
	assert.Zero(t, countRows(t, database, ctx, `SELECT count(*) FROM app.subscriptions`))
}
