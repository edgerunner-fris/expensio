package billing

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

// newSubscriptionEvent 用落库后的订阅行构造事件。
// OccurredAt 取数据库写入的 updated_at，而不是服务进程时钟。
func newSubscriptionEvent(t events.Type, s *Subscription) (events.Envelope, error) {
	snap := events.SubscriptionSnapshot{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		PlanCode:           s.PlanCode,
		Seats:              s.Seats,
		Status:             string(s.Status),
		CurrentPeriodStart: s.CurrentPeriodStart,
		CurrentPeriodEnd:   s.CurrentPeriodEnd,
		Version:            s.Version,
		CanceledAt:         s.CanceledAt,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}
	if err := snap.Validate(); err != nil {
		return events.Envelope{}, fmt.Errorf("billing: 订阅快照非法: %w", err)
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		return events.Envelope{}, fmt.Errorf("billing: 序列化订阅快照失败: %w", err)
	}
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             t,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         s.TenantID,
		AggregateID:      s.ID,
		AggregateVersion: s.Version,
		OccurredAt:       s.UpdatedAt,
		Payload:          payload,
	}, nil
}

func newInvoiceEvent(t events.Type, inv *Invoice, occurredAt time.Time) (events.Envelope, error) {
	snap := events.InvoiceSnapshot{
		ID:             inv.ID,
		TenantID:       inv.TenantID,
		SubscriptionID: inv.SubscriptionID,
		Number:         inv.Number,
		Status:         string(inv.Status),
		PeriodStart:    inv.PeriodStart,
		PeriodEnd:      inv.PeriodEnd,
		Currency:       string(inv.Currency),
		AmountMinor:    inv.AmountMinor,
		DueOn:          inv.DueOn,
		IssuedAt:       inv.IssuedAt,
		PaidAt:         inv.PaidAt,
		Version:        inv.Version,
	}
	if err := snap.Validate(); err != nil {
		return events.Envelope{}, fmt.Errorf("billing: 账单快照非法: %w", err)
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		return events.Envelope{}, fmt.Errorf("billing: 序列化账单快照失败: %w", err)
	}
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             t,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         inv.TenantID,
		AggregateID:      inv.ID,
		AggregateVersion: inv.Version,
		OccurredAt:       occurredAt,
		Payload:          payload,
	}, nil
}
