package expense

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/money"
)

// Repository 只负责 SQL，不做权限判断，也不开事务。
// 事务边界由 service 决定：草稿修改与审批预占都必须和状态变更在同一个事务里，
// 如果仓储自己开事务，service 就再也拼不回这个边界。
type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

// expenseColumns 显式列出所有列。
// SELECT * 会让"加一列"悄悄改变扫描顺序，也可能把未来加入的敏感列带进响应。
const expenseColumns = `id, tenant_id, submitter_id, status, category, currency,
	amount_minor, merchant, description, occurred_at, budget_id, decided_by,
	decided_at, rejection_reason, version, required_approval_levels, approval_level,
	created_at, updated_at, created_by, updated_by, deleted_at`

// scanExpense 用一个中间字符串接住枚举列，再转成领域类型。
// 数据库的 CHECK 保证取值合法；这里不做二次校验，避免读路径因为脏数据整页失败。
func scanExpense(row pgx.Row) (*Expense, error) {
	var (
		e                          Expense
		status, category, currency string
	)
	err := row.Scan(
		&e.ID, &e.TenantID, &e.SubmitterID, &status, &category, &currency,
		&e.AmountMinor, &e.Merchant, &e.Description, &e.OccurredAt, &e.BudgetID,
		&e.DecidedBy, &e.DecidedAt, &e.RejectionReason, &e.Version,
		&e.RequiredApprovalLevels, &e.ApprovalLevel,
		&e.CreatedAt, &e.UpdatedAt, &e.CreatedBy, &e.UpdatedBy, &e.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	e.Status = Status(status)
	e.Category = Category(category)
	e.Currency = money.Currency(currency)
	return &e, nil
}

// Create 插入一张草稿。status 由触发器强制为 draft，
// 因此这里不需要（也不允许）传入任意状态。
func (r *Repository) Create(ctx context.Context, tx pgx.Tx, e *Expense) error {
	const q = `INSERT INTO app.expenses
	    (id, tenant_id, submitter_id, status, category, currency, amount_minor,
	     merchant, description, occurred_at, budget_id, version, created_by, updated_by)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
	 RETURNING created_at, updated_at`
	return tx.QueryRow(ctx, q,
		e.ID, e.TenantID, e.SubmitterID, string(e.Status), string(e.Category),
		string(e.Currency), e.AmountMinor, e.Merchant, e.Description, e.OccurredAt,
		e.BudgetID, e.Version, e.CreatedBy,
	).Scan(&e.CreatedAt, &e.UpdatedAt)
}

// Get 按 ID 取一张未删除的费用单。RLS 保证跨租户不可见；
// 租户内是否能看，由 service 用 CanView 判定。
func (r *Repository) Get(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Expense, error) {
	return scanExpense(tx.QueryRow(ctx,
		`SELECT `+expenseColumns+` FROM app.expenses WHERE id = $1 AND deleted_at IS NULL`, id))
}

// UpdateDraft 以乐观锁更新草稿的可编辑字段。
//
// WHERE 里的 status/version/deleted_at 三个条件缺一不可：
// 少了 status，已提交的单据会被改金额；少了 version，两个标签页的修改会互相覆盖。
// 影响 0 行时返回 pgx.ErrNoRows，由 service 翻译成 409。
func (r *Repository) UpdateDraft(ctx context.Context, tx pgx.Tx, e *Expense) error {
	const q = `UPDATE app.expenses
	 SET category = $1, currency = $2, amount_minor = $3, merchant = $4,
	     description = $5, occurred_at = $6, budget_id = $7,
	     version = version + 1, updated_by = $8
	 WHERE id = $9 AND tenant_id = $10 AND version = $11
	   AND status = 'draft' AND deleted_at IS NULL
	 RETURNING version, updated_at, updated_by`
	return tx.QueryRow(ctx, q,
		string(e.Category), string(e.Currency), e.AmountMinor, e.Merchant,
		e.Description, e.OccurredAt, e.BudgetID, e.UpdatedBy,
		e.ID, e.TenantID, e.Version,
	).Scan(&e.Version, &e.UpdatedAt, &e.UpdatedBy)
}

// SoftDeleteDraft 软删草稿，同样带乐观锁。
// 已提交的单据不能删，这一点由 WHERE status='draft' 与数据库触发器双重保证。
func (r *Repository) SoftDeleteDraft(ctx context.Context, tx pgx.Tx, id, tenantID, actor uuid.UUID, version int) error {
	const q = `UPDATE app.expenses
	 SET deleted_at = now(), version = version + 1, updated_by = $2
	 WHERE id = $1 AND tenant_id = $3 AND version = $4
	   AND status = 'draft' AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, id, actor, tenantID, version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ListFilter 是列表查询的数据库侧条件。
// SubmitterID 为空表示全租户；是否允许全租户在 service 里已由 ResolveScope 决定。
// InboxActor 非空时只返回该用户待审批的单：状态 submitted、不是自己提交的、
// 且自己还没有批准过任何一级（多级审批要求不同人）。
type ListFilter struct {
	SubmitterID uuid.UUID
	Status      Status
	InboxActor  uuid.UUID
	Limit       int
	Offset      int
}

// List 返回当前租户（RLS 过滤）下的费用单与总数。
// 分页用 OFFSET：M2 的数据量级不需要 keyset，但排序里带上 id 保证翻页稳定。
func (r *Repository) List(ctx context.Context, tx pgx.Tx, f ListFilter) ([]Expense, int64, error) {
	var submitter any
	if f.SubmitterID != uuid.Nil {
		submitter = f.SubmitterID
	}
	var status any
	if f.Status != "" {
		status = string(f.Status)
	}
	var inboxActor any
	if f.InboxActor != uuid.Nil {
		inboxActor = f.InboxActor
	}

	// 用 NULL 参数表达"不过滤"，避免把用户输入拼进 SQL。
	const where = `deleted_at IS NULL
		AND ($1::uuid IS NULL OR submitter_id = $1)
		AND ($2::text IS NULL OR status = $2)
		AND ($3::uuid IS NULL OR (
			submitter_id <> $3
			AND NOT EXISTS (
				SELECT 1 FROM app.expense_approvals a
				WHERE a.tenant_id = app.expenses.tenant_id
				  AND a.expense_id = app.expenses.id
				  AND a.actor_id = $3
			)
		))`

	var total int64
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM app.expenses WHERE `+where, submitter, status, inboxActor).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := tx.Query(ctx,
		`SELECT `+expenseColumns+` FROM app.expenses WHERE `+where+`
		 ORDER BY created_at DESC, id DESC LIMIT $4 OFFSET $5`,
		submitter, status, inboxActor, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Expense, 0, f.Limit)
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
