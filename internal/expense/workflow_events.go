package expense

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

// actionEventTypes 是审批动作与事件类型的唯一映射。
// 状态机新增边时必须同时在这里补事件，否则 report 会静默漏掉一类变化。
var actionEventTypes = map[Action]events.Type{
	ActionSubmit:    events.TypeExpenseSubmitted,
	ActionApprove:   events.TypeExpenseApproved,
	ActionReject:    events.TypeExpenseRejected,
	ActionCancel:    events.TypeExpenseCancelled,
	ActionReimburse: events.TypeExpenseReimbursed,
}

// newExpenseEvent 用动作完成后的费用行构造事件。
// OccurredAt 取数据库写入的 updated_at：它是这次状态变化真正落库的时间，
// 服务进程的时钟再准也只用于业务字段（decided_at）。
func newExpenseEvent(action Action, e *Expense) (events.Envelope, error) {
	eventType, ok := actionEventTypes[action]
	if !ok {
		return events.Envelope{}, fmt.Errorf("expense: 动作 %s 没有对应事件", action)
	}
	snapshot := events.ExpenseSnapshot{
		ID:          e.ID,
		TenantID:    e.TenantID,
		SubmitterID: e.SubmitterID,
		Status:      string(e.Status),
		Category:    string(e.Category),
		Currency:    string(e.Currency),
		AmountMinor: e.AmountMinor,
		OccurredAt:  e.OccurredAt,
		BudgetID:    e.BudgetID,
		DecidedBy:   e.DecidedBy,
		DecidedAt:   e.DecidedAt,
		Version:     e.Version,
		CreatedAt:   e.CreatedAt,
		UpdatedAt:   e.UpdatedAt,
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return events.Envelope{}, fmt.Errorf("expense: 序列化事件快照失败: %w", err)
	}
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             eventType,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         e.TenantID,
		AggregateID:      e.ID,
		AggregateVersion: e.Version,
		OccurredAt:       e.UpdatedAt,
		Payload:          payload,
	}, nil
}
