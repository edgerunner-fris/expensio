package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/money"
)

const planColumns = `code, name, price_minor, currency, interval`

// Repository 是 billing 的 PostgreSQL 读写层，不持有连接：
// 事务边界在 Service，仓储只在传入的 tx 上执行。
type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

func scanPlan(row pgx.Row) (*Plan, error) {
	var (
		p                  Plan
		currency, interval string
	)
	if err := row.Scan(&p.Code, &p.Name, &p.PriceMinor, &currency, &interval); err != nil {
		return nil, err
	}
	p.Currency = money.Currency(currency)
	p.Interval = PlanInterval(interval)
	return &p, nil
}

// ListPlans 返回全局套餐目录。plans 没有 RLS：目录对所有租户相同，
// 但运行时角色只有 SELECT 权限，改价必须走迁移。
func (r *Repository) ListPlans(ctx context.Context, tx pgx.Tx) ([]Plan, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+planColumns+` FROM app.plans ORDER BY interval, price_minor, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Plan, 0, 8)
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *p)
	}
	return items, rows.Err()
}

func (r *Repository) GetPlan(ctx context.Context, tx pgx.Tx, code string) (*Plan, error) {
	return scanPlan(tx.QueryRow(ctx,
		`SELECT `+planColumns+` FROM app.plans WHERE code = $1`, code))
}

const subscriptionColumns = `id, tenant_id, plan_code, seats, status,
	current_period_start, current_period_end, version, canceled_at,
	created_at, updated_at, created_by, updated_by`

func scanSubscription(row pgx.Row) (*Subscription, error) {
	var (
		s      Subscription
		status string
	)
	if err := row.Scan(
		&s.ID, &s.TenantID, &s.PlanCode, &s.Seats, &status,
		&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.Version, &s.CanceledAt,
		&s.CreatedAt, &s.UpdatedAt, &s.CreatedBy, &s.UpdatedBy,
	); err != nil {
		return nil, err
	}
	s.Status = SubscriptionStatus(status)
	return &s, nil
}

// CreateSubscription 插入一条订阅。同一租户第二条未取消订阅由部分唯一索引拒绝，
// 这里不做"先查再插"：那会在并发下漏过去。
func (r *Repository) CreateSubscription(ctx context.Context, tx pgx.Tx, s *Subscription) error {
	const q = `INSERT INTO app.subscriptions
	    (id, tenant_id, plan_code, seats, status, current_period_start, current_period_end,
	     version, created_by, updated_by)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8, $8)
	 RETURNING version, created_at, updated_at`
	return tx.QueryRow(ctx, q,
		s.ID, s.TenantID, s.PlanCode, s.Seats, string(s.Status),
		dateString(s.CurrentPeriodStart), dateString(s.CurrentPeriodEnd),
		s.CreatedBy,
	).Scan(&s.Version, &s.CreatedAt, &s.UpdatedAt)
}

// GetSubscription 按 ID 取订阅；forUpdate 用于取消这类"先读后写"的路径。
func (r *Repository) GetSubscription(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (*Subscription, error) {
	q := `SELECT ` + subscriptionColumns + ` FROM app.subscriptions WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanSubscription(tx.QueryRow(ctx, q, id))
}

// GetLiveSubscription 返回当前未取消的订阅。
// 部分唯一索引保证最多一条，因此不需要额外的排序消歧。
func (r *Repository) GetLiveSubscription(ctx context.Context, tx pgx.Tx) (*Subscription, error) {
	const q = `SELECT ` + subscriptionColumns + ` FROM app.subscriptions
	 WHERE status <> 'canceled' ORDER BY created_at DESC LIMIT 1`
	return scanSubscription(tx.QueryRow(ctx, q))
}

func (r *Repository) ListSubscriptions(ctx context.Context, tx pgx.Tx, limit, offset int) ([]Subscription, int64, error) {
	var total int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.subscriptions`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT `+subscriptionColumns+` FROM app.subscriptions
		 ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Subscription, 0, limit)
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// CancelSubscription 把订阅置为终态。状态白名单由数据库触发器再校验一次。
func (r *Repository) CancelSubscription(ctx context.Context, tx pgx.Tx, s *Subscription, expectedVersion int) error {
	const q = `UPDATE app.subscriptions
	 SET status = 'canceled', canceled_at = $1, updated_by = $2
	 WHERE id = $3 AND tenant_id = $4 AND version = $5 AND status <> 'canceled'
	 RETURNING status, version, canceled_at, updated_at`
	var status string
	if err := tx.QueryRow(ctx, q,
		s.CanceledAt, s.UpdatedBy, s.ID, s.TenantID, expectedVersion,
	).Scan(&status, &s.Version, &s.CanceledAt, &s.UpdatedAt); err != nil {
		return err
	}
	s.Status = SubscriptionStatus(status)
	return nil
}

// dateString 把日期统一成 date 列接受的字符串，避免驱动按本地时区解释 time.Time。
func dateString(t time.Time) string {
	return t.Format("2006-01-02")
}

const invoiceColumns = `id, tenant_id, subscription_id, number, status,
	period_start, period_end, currency, amount_minor, due_on, issued_at, paid_at, version`

func scanInvoice(row pgx.Row) (*Invoice, error) {
	var (
		inv      Invoice
		status   string
		currency string
	)
	if err := row.Scan(
		&inv.ID, &inv.TenantID, &inv.SubscriptionID, &inv.Number, &status,
		&inv.PeriodStart, &inv.PeriodEnd, &currency, &inv.AmountMinor,
		&inv.DueOn, &inv.IssuedAt, &inv.PaidAt, &inv.Version,
	); err != nil {
		return nil, err
	}
	inv.Status = InvoiceStatus(status)
	inv.Currency = money.Currency(currency)
	return &inv, nil
}

// CreateInvoice 插入账单。同一订阅周期重复出账由唯一约束拒绝，
// 金额与周期在插入时就固定，后续改价不会回溯影响历史账单。
func (r *Repository) CreateInvoice(ctx context.Context, tx pgx.Tx, inv *Invoice) error {
	const q = `INSERT INTO app.invoices
	    (id, tenant_id, subscription_id, number, status, period_start, period_end,
	     currency, amount_minor, due_on)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	 RETURNING version, issued_at`
	return tx.QueryRow(ctx, q,
		inv.ID, inv.TenantID, inv.SubscriptionID, inv.Number, string(inv.Status),
		dateString(inv.PeriodStart), dateString(inv.PeriodEnd),
		string(inv.Currency), inv.AmountMinor, dateString(inv.DueOn),
	).Scan(&inv.Version, &inv.IssuedAt)
}

func (r *Repository) GetInvoice(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Invoice, error) {
	return scanInvoice(tx.QueryRow(ctx,
		`SELECT `+invoiceColumns+` FROM app.invoices WHERE id = $1`, id))
}

// InvoiceFilter 里没有租户字段：租户只来自 RLS 上下文。
type InvoiceFilter struct {
	Status InvoiceStatus
	Limit  int
	Offset int
}

func (r *Repository) ListInvoices(ctx context.Context, tx pgx.Tx, f InvoiceFilter) ([]Invoice, int64, error) {
	var status any
	if f.Status != "" {
		status = string(f.Status)
	}
	const where = `($1::text IS NULL OR status = $1)`

	var total int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM app.invoices WHERE `+where, status).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT `+invoiceColumns+` FROM app.invoices WHERE `+where+`
		 ORDER BY issued_at DESC, id DESC LIMIT $2 OFFSET $3`,
		status, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Invoice, 0, f.Limit)
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *inv)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
