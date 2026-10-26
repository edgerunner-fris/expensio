package expense

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

// maxCommentRunes 与 approval_steps_comment_check 一致。
const maxCommentRunes = 1000

// ApplyAction 执行一次审批动作。整个动作在一个事务里完成：
//
//	submit    : 匹配预算 → 条件预占 → 改状态 → 追加轨迹 → 写 outbox
//	approve   : reserved → spent  → 改状态 → 追加轨迹 → 写 outbox
//	reject    : 释放预占        → 改状态 → 追加轨迹 → 写 outbox
//	cancel    : 释放预占        → 改状态 → 追加轨迹 → 写 outbox
//	reimburse : 改状态（预算不变）→ 追加轨迹 → 写 outbox
//
// 任何一步失败都会整体回滚，不会留下"钱占了但单没提交""单批了但钱没扣"
// 或"业务成功但事件丢失"的状态。Kafka 发送不在事务内，见 ADR-0004。
func (s *Service) ApplyAction(ctx context.Context, actor Actor, id uuid.UUID, action Action, comment string) (*Expense, error) {
	if !action.Valid() {
		return nil, apperr.InvalidArgument("未知的审批动作")
	}
	comment = strings.TrimSpace(comment)
	if utf8.RuneCountInString(comment) > maxCommentRunes {
		return nil, apperr.InvalidArgument("审批意见不超过 1000 个字符")
	}
	if action == ActionReject && comment == "" {
		return nil, apperr.InvalidArgument("驳回必须填写理由")
	}

	var out *Expense
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// 先锁单，再校验：锁内的状态才是本次动作看到的状态。
		e, err := s.repo.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		to, err := AuthorizeAction(actor, e, action)
		if err != nil {
			return err
		}

		switch action {
		case ActionSubmit:
			budgetID, err := s.resolveBudget(ctx, tx, e)
			if err != nil {
				return err
			}
			if budgetID != nil {
				if err := s.repo.Reserve(ctx, tx, e.TenantID, *budgetID, e.AmountMinor); err != nil {
					if isNoRows(err) {
						return apperr.Conflict(CodeBudgetUnavailable, "预算可用额度不足")
					}
					return err
				}
			}
			out, err = s.repo.Submit(ctx, tx, e, budgetID, actor.UserID)
			if err != nil {
				return stateConflictUnlessNoRows(err, e, action)
			}

		case ActionApprove:
			if e.BudgetID != nil {
				if err := s.repo.CommitReservation(ctx, tx, e.TenantID, *e.BudgetID, e.AmountMinor); err != nil {
					if isNoRows(err) {
						return apperr.Conflict(CodeBudgetUnavailable, "预算预占不足，无法结转")
					}
					return err
				}
			}
			out, err = s.repo.Approve(ctx, tx, e, actor.UserID, s.now())
			if err != nil {
				return stateConflictUnlessNoRows(err, e, action)
			}

		case ActionReject:
			if err := s.releaseBudget(ctx, tx, e); err != nil {
				return err
			}
			out, err = s.repo.Reject(ctx, tx, e, actor.UserID, s.now(), comment)
			if err != nil {
				return stateConflictUnlessNoRows(err, e, action)
			}

		case ActionCancel:
			if err := s.releaseBudget(ctx, tx, e); err != nil {
				return err
			}
			out, err = s.repo.Cancel(ctx, tx, e, actor.UserID)
			if err != nil {
				return stateConflictUnlessNoRows(err, e, action)
			}

		case ActionReimburse:
			out, err = s.repo.Reimburse(ctx, tx, e, actor.UserID)
			if err != nil {
				return stateConflictUnlessNoRows(err, e, action)
			}
		}

		if err := s.repo.InsertApprovalStep(ctx, tx, &ApprovalStep{
			ID:         uuid.New(),
			TenantID:   e.TenantID,
			ExpenseID:  e.ID,
			ActorID:    actor.UserID,
			Action:     action,
			FromStatus: e.Status,
			ToStatus:   to,
			Comment:    comment,
		}); err != nil {
			return err
		}

		env, err := newExpenseEvent(action, out)
		if err != nil {
			return err
		}
		return s.outbox.Insert(ctx, tx, events.TopicExpense, env)
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// releaseBudget 在驳回/撤销时释放预占。无预算的单据直接跳过。
func (s *Service) releaseBudget(ctx context.Context, tx pgx.Tx, e *Expense) error {
	if e.BudgetID == nil {
		return nil
	}
	if err := s.repo.Release(ctx, tx, e.TenantID, *e.BudgetID, e.AmountMinor); err != nil {
		if isNoRows(err) {
			return apperr.Conflict(CodeBudgetUnavailable, "预算预占不足，无法释放")
		}
		return err
	}
	return nil
}

// stateConflictUnlessNoRows 把条件 UPDATE 未命中翻译成状态冲突。
// 其他错误原样返回，交给 mapDBError。
func stateConflictUnlessNoRows(err error, e *Expense, action Action) error {
	if isNoRows(err) {
		return ErrStateConflict(e.Status, action)
	}
	return err
}
