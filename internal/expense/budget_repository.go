package expense

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/money"
)

const budgetColumns = `id, tenant_id, name, category, currency, period_start, period_end,
	amount_minor, reserved_minor, spent_minor, version, created_at, updated_at,
	created_by, updated_by, deleted_at`

func scanBudget(row pgx.Row) (*Budget, error) {
	var (
		b                  Budget
		category, currency string
	)
	err := row.Scan(
		&b.ID, &b.TenantID, &b.Name, &category, &currency, &b.PeriodStart, &b.PeriodEnd,
		&b.AmountMinor, &b.ReservedMinor, &b.SpentMinor, &b.Version,
		&b.CreatedAt, &b.UpdatedAt, &b.CreatedBy, &b.UpdatedBy, &b.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	b.Category = Category(category)
	b.Currency = money.Currency(currency)
	return &b, nil
}

func (r *Repository) CreateBudget(ctx context.Context, tx pgx.Tx, b *Budget) error {
	const q = `INSERT INTO app.budgets
	    (id, tenant_id, name, category, currency, period_start, period_end,
	     amount_minor, version, created_by, updated_by)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $9, $9)
	 RETURNING version, created_at, updated_at`
	return tx.QueryRow(ctx, q,
		b.ID, b.TenantID, b.Name, string(b.Category), string(b.Currency),
		dateString(b.PeriodStart), dateString(b.PeriodEnd), b.AmountMinor,
		b.CreatedBy,
	).Scan(&b.Version, &b.CreatedAt, &b.UpdatedAt)
}

// GetBudget 按 ID 取未删除的预算。
// forUpdate 为 true 时加行锁：修改金额/期间前必须先拿到最新的 reserved/spent，
// 否则"检查时够、写入时不够"会在并发下漏过去。
func (r *Repository) GetBudget(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (*Budget, error) {
	q := `SELECT ` + budgetColumns + ` FROM app.budgets WHERE id = $1 AND deleted_at IS NULL`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return scanBudget(tx.QueryRow(ctx, q, id))
}

// UpdateBudget 更新可编辑字段。类别与币种不可改：它们是"这条预算匹配哪些费用"的身份。
func (r *Repository) UpdateBudget(ctx context.Context, tx pgx.Tx, b *Budget, expectedVersion int) error {
	const q = `UPDATE app.budgets
	 SET name = $1, amount_minor = $2, period_start = $3, period_end = $4, updated_by = $5
	 WHERE id = $6 AND tenant_id = $7 AND version = $8 AND deleted_at IS NULL
	 RETURNING version, updated_at`
	return tx.QueryRow(ctx, q,
		b.Name, b.AmountMinor, dateString(b.PeriodStart), dateString(b.PeriodEnd),
		b.UpdatedBy, b.ID, b.TenantID, expectedVersion,
	).Scan(&b.Version, &b.UpdatedAt)
}

func (r *Repository) SoftDeleteBudget(ctx context.Context, tx pgx.Tx, id, tenantID, actor uuid.UUID, expectedVersion int) error {
	const q = `UPDATE app.budgets
	 SET deleted_at = now(), updated_by = $2
	 WHERE id = $1 AND tenant_id = $3 AND version = $4 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, actor, tenantID, expectedVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

type BudgetFilter struct {
	Category Category
	Limit    int
	Offset   int
}

func (r *Repository) ListBudgets(ctx context.Context, tx pgx.Tx, f BudgetFilter) ([]Budget, int64, error) {
	var category any
	if f.Category != "" {
		category = string(f.Category)
	}
	const where = `deleted_at IS NULL AND ($1::text IS NULL OR category = $1)`

	var total int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM app.budgets WHERE `+where, category).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT `+budgetColumns+` FROM app.budgets WHERE `+where+`
		 ORDER BY period_start DESC, id DESC LIMIT $2 OFFSET $3`,
		category, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Budget, 0, f.Limit)
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// HasExpenses 判断预算是否已经被任一费用单引用（草稿也算）。
// 有引用时不允许改期间：否则已归入该预算的费用会落到期间之外。
func (r *Repository) HasExpenses(ctx context.Context, tx pgx.Tx, budgetID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.expenses WHERE budget_id = $1)`, budgetID).Scan(&exists)
	return exists, err
}

// HasAnyBudget 表示租户是否配置过任何有效预算。
// 提交时的规则依赖它：有预算就必须匹配，从未配置过才允许不带预算。
func (r *Repository) HasAnyBudget(ctx context.Context, tx pgx.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.budgets WHERE deleted_at IS NULL)`).Scan(&exists)
	return exists, err
}

// MatchBudget 找出发生日期落在期间内、类别与币种一致的预算。
// 排斥约束保证同一 (类别, 币种, 日期) 最多一条，因此不需要在 Go 里再消歧。
//
// 日期按 UTC 解释：occurred_at 是 timestamptz，而预算期间是 date。
// 若不固定时区，同一张单在不同服务器时区下可能匹配到不同预算。
func (r *Repository) MatchBudget(ctx context.Context, tx pgx.Tx, category Category, currency money.Currency, occurredAt time.Time) (*Budget, error) {
	const q = `SELECT ` + budgetColumns + ` FROM app.budgets
	 WHERE deleted_at IS NULL
	   AND category = $1 AND currency = $2
	   AND period_start <= ($3::timestamptz AT TIME ZONE 'UTC')::date
	   AND period_end >= ($3::timestamptz AT TIME ZONE 'UTC')::date
	 ORDER BY period_start DESC
	 LIMIT 1`
	return scanBudget(tx.QueryRow(ctx, q, string(category), string(currency), occurredAt))
}

// dateString 把日期统一格式化成 date 列接受的字符串。
// 不直接传 time.Time：那会把"本地时区的零点"交给驱动去猜，跨时区时可能变成前一天。
func dateString(t time.Time) string {
	return t.Format("2006-01-02")
}
