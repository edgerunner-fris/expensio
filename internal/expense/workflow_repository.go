package expense

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// GetForUpdate 取一张费用单并加行锁。
// 审批的每一步都必须先锁单：没有行锁，"检查状态"和"改状态"之间会插进另一个请求。
func (r *Repository) GetForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Expense, error) {
	return scanExpense(tx.QueryRow(ctx,
		`SELECT `+expenseColumns+` FROM app.expenses
		 WHERE id = $1 AND deleted_at IS NULL
		 FOR UPDATE`, id))
}

// Submit 把草稿改为已提交，并写入最终匹配的预算与审批级数。
// WHERE 里带 status='draft'：即使调用方漏了校验，也不会把已审批的单改回提交态。
func (r *Repository) Submit(ctx context.Context, tx pgx.Tx, e *Expense, budgetID *uuid.UUID, actor uuid.UUID, requiredLevels int16) (*Expense, error) {
	const q = `UPDATE app.expenses
	 SET status = 'submitted', budget_id = $1, required_approval_levels = $2,
	     version = version + 1, updated_by = $3
	 WHERE id = $4 AND tenant_id = $5 AND status = 'draft' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, budgetID, requiredLevels, actor, e.ID, e.TenantID))
}

func (r *Repository) Approve(ctx context.Context, tx pgx.Tx, e *Expense, actor uuid.UUID, at time.Time) (*Expense, error) {
	// 最后一级批准同时把 counter 推到 required，满足
	// expenses_approval_level_consistency（approved ⇔ 走完全部级别）。
	const q = `UPDATE app.expenses
	 SET status = 'approved', decided_by = $1, decided_at = $2,
	     approval_level = required_approval_levels,
	     version = version + 1, updated_by = $1
	 WHERE id = $3 AND tenant_id = $4 AND status = 'submitted' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, actor, at, e.ID, e.TenantID))
}

// AdvanceApprovalLevel 只推进计数器，不改状态——中间级批准的唯一效果。
func (r *Repository) AdvanceApprovalLevel(ctx context.Context, tx pgx.Tx, e *Expense, level int16, actor uuid.UUID) (*Expense, error) {
	const q = `UPDATE app.expenses
	 SET approval_level = $1, version = version + 1, updated_by = $2
	 WHERE id = $3 AND tenant_id = $4 AND status = 'submitted' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, level, actor, e.ID, e.TenantID))
}

// RequiredApprovalLevels 在提交时计算审批级数。
// 策略按租户 + 币种匹配；没有策略或金额低于门槛即单级。
func (r *Repository) RequiredApprovalLevels(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, currency string, amountMinor int64) (int16, error) {
	var levels int16
	err := tx.QueryRow(ctx, `
		SELECT CASE WHEN EXISTS (
			SELECT 1 FROM app.approval_policies
			WHERE tenant_id = $1 AND currency = $2
			  AND second_level_threshold_minor <= $3
		) THEN 2 ELSE 1 END`,
		tenantID, currency, amountMinor).Scan(&levels)
	return levels, err
}

// InsertApproval 追加一级批准。level 从 1 开始逐级递增，
// 同人/同级重复由唯一约束与触发器拒绝。
func (r *Repository) InsertApproval(ctx context.Context, tx pgx.Tx, expenseID, tenantID uuid.UUID, level int16, actorID uuid.UUID, comment string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO app.expense_approvals
		    (id, tenant_id, expense_id, level, actor_id, comment)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), tenantID, expenseID, level, actorID, comment)
	return err
}

// ListApprovals 返回一张单的各级批准记录，按级别升序。
func (r *Repository) ListApprovals(ctx context.Context, tx pgx.Tx, tenantID, expenseID uuid.UUID) ([]ApprovalRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, expense_id, level, actor_id, comment, created_at
		FROM app.expense_approvals
		WHERE tenant_id = $1 AND expense_id = $2
		ORDER BY level`, tenantID, expenseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ApprovalRecord, 0, 2)
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ID, &a.TenantID, &a.ExpenseID, &a.Level,
			&a.ActorID, &a.Comment, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) Reject(ctx context.Context, tx pgx.Tx, e *Expense, actor uuid.UUID, at time.Time, reason string) (*Expense, error) {
	const q = `UPDATE app.expenses
	 SET status = 'rejected', decided_by = $1, decided_at = $2, rejection_reason = $3,
	     version = version + 1, updated_by = $1
	 WHERE id = $4 AND tenant_id = $5 AND status = 'submitted' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, actor, at, reason, e.ID, e.TenantID))
}

// Cancel 不写 decided_*：数据库的 decision 一致性约束要求 cancelled 状态没有审批人。
func (r *Repository) Cancel(ctx context.Context, tx pgx.Tx, e *Expense, actor uuid.UUID) (*Expense, error) {
	const q = `UPDATE app.expenses
	 SET status = 'cancelled', version = version + 1, updated_by = $1
	 WHERE id = $2 AND tenant_id = $3 AND status = 'submitted' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, actor, e.ID, e.TenantID))
}

// Reimburse 保留原有的 decided_by/decided_at：报销人不是审批人，不能覆盖审批记录。
func (r *Repository) Reimburse(ctx context.Context, tx pgx.Tx, e *Expense, actor uuid.UUID) (*Expense, error) {
	const q = `UPDATE app.expenses
	 SET status = 'reimbursed', version = version + 1, updated_by = $1
	 WHERE id = $2 AND tenant_id = $3 AND status = 'approved' AND deleted_at IS NULL
	 RETURNING ` + expenseColumns
	return scanExpense(tx.QueryRow(ctx, q, actor, e.ID, e.TenantID))
}

// InsertApprovalStep 追加一条审批轨迹。表本身只允许 INSERT（REVOKE UPDATE/DELETE）。
func (r *Repository) InsertApprovalStep(ctx context.Context, tx pgx.Tx, s *ApprovalStep) error {
	const q = `INSERT INTO app.approval_steps
	    (id, tenant_id, expense_id, actor_id, action, from_status, to_status, comment)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := tx.Exec(ctx, q,
		s.ID, s.TenantID, s.ExpenseID, s.ActorID, string(s.Action),
		string(s.FromStatus), string(s.ToStatus), s.Comment)
	return err
}

// Reserve 条件预占：额度不足时影响 0 行。
// 金额比较与写入在同一条 UPDATE 里，所以并发提交不会"都查到够、都扣成功"。
func (r *Repository) Reserve(ctx context.Context, tx pgx.Tx, tenantID, budgetID uuid.UUID, amount int64) error {
	const q = `UPDATE app.budgets
	 SET reserved_minor = reserved_minor + $1
	 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
	   AND reserved_minor + spent_minor + $1 <= amount_minor`
	tag, err := tx.Exec(ctx, q, amount, budgetID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Release 释放预占（驳回/撤销）。reserved_minor >= amount 防止把预算扣成负数。
func (r *Repository) Release(ctx context.Context, tx pgx.Tx, tenantID, budgetID uuid.UUID, amount int64) error {
	const q = `UPDATE app.budgets
	 SET reserved_minor = reserved_minor - $1
	 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
	   AND reserved_minor >= $1`
	tag, err := tx.Exec(ctx, q, amount, budgetID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CommitReservation 把预占转为支出（审批通过）。reserved → spent 是同一个 UPDATE，
// 不会出现"预占已减、支出未加"的中间态。
func (r *Repository) CommitReservation(ctx context.Context, tx pgx.Tx, tenantID, budgetID uuid.UUID, amount int64) error {
	const q = `UPDATE app.budgets
	 SET reserved_minor = reserved_minor - $1,
	     spent_minor = spent_minor + $1
	 WHERE id = $2 AND tenant_id = $3 AND deleted_at IS NULL
	   AND reserved_minor >= $1`
	tag, err := tx.Exec(ctx, q, amount, budgetID, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
