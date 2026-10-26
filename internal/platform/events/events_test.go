package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

var fixedTime = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func validSnapshot() events.ExpenseSnapshot {
	return events.ExpenseSnapshot{
		ID:          uuid.New(),
		TenantID:    uuid.New(),
		SubmitterID: uuid.New(),
		Status:      "submitted",
		Category:    "travel",
		Currency:    "USD",
		AmountMinor: 12345,
		OccurredAt:  fixedTime.Add(-24 * time.Hour),
		Version:     2,
		CreatedAt:   fixedTime.Add(-2 * time.Hour),
		UpdatedAt:   fixedTime.Add(-time.Hour),
	}
}

func validEnvelope(t *testing.T) events.Envelope {
	t.Helper()
	tenantID := uuid.New()
	snap := validSnapshot()
	snap.TenantID = tenantID
	raw, err := json.Marshal(snap)
	require.NoError(t, err)
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             events.TypeExpenseSubmitted,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         tenantID,
		AggregateID:      snap.ID,
		AggregateVersion: snap.Version,
		OccurredAt:       fixedTime,
		Payload:          raw,
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := validEnvelope(t)

	raw, err := in.Encode()
	require.NoError(t, err)
	require.True(t, json.Valid(raw), "编码结果必须是合法 JSON")

	out, err := events.Decode(raw)
	require.NoError(t, err)
	assert.Equal(t, in.EventID, out.EventID)
	assert.Equal(t, in.Type, out.Type)
	assert.Equal(t, in.SchemaVersion, out.SchemaVersion)
	assert.Equal(t, in.TenantID, out.TenantID)
	assert.Equal(t, in.AggregateID, out.AggregateID)
	assert.Equal(t, in.AggregateVersion, out.AggregateVersion)
	assert.True(t, in.OccurredAt.Equal(out.OccurredAt))
	assert.JSONEq(t, string(in.Payload), string(out.Payload))

	var snap events.ExpenseSnapshot
	require.NoError(t, json.Unmarshal(out.Payload, &snap))
	assert.Equal(t, in.TenantID, snap.TenantID, "payload 与信封必须描述同一租户")
	assert.NoError(t, snap.Validate())
}

func TestPublishKeyIsTenant(t *testing.T) {
	env := validEnvelope(t)
	assert.Equal(t, env.TenantID.String(), env.PublishKey(),
		"同一租户必须落到同一分区，否则同单据事件会被乱序消费")
}

func TestEnvelopeValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*events.Envelope)
	}{
		{"缺少事件 ID", func(e *events.Envelope) { e.EventID = uuid.Nil }},
		{"未知事件类型", func(e *events.Envelope) { e.Type = "expense.refunded" }},
		{"schema 版本不匹配", func(e *events.Envelope) { e.SchemaVersion = 2 }},
		{"缺少租户", func(e *events.Envelope) { e.TenantID = uuid.Nil }},
		{"缺少聚合 ID", func(e *events.Envelope) { e.AggregateID = uuid.Nil }},
		{"聚合版本为 0", func(e *events.Envelope) { e.AggregateVersion = 0 }},
		{"缺少发生时间", func(e *events.Envelope) { e.OccurredAt = time.Time{} }},
		{"缺少 payload", func(e *events.Envelope) { e.Payload = nil }},
		{"payload 非法 JSON", func(e *events.Envelope) { e.Payload = json.RawMessage(`{"a":`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope(t)
			tc.mutate(&env)
			_, err := env.Encode()
			require.Error(t, err)
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	_, err := events.Decode([]byte("not json"))
	require.Error(t, err)

	_, err = events.Decode([]byte(`{"event_id":"00000000-0000-0000-0000-000000000000"}`))
	require.Error(t, err)
}

func TestSnapshotValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*events.ExpenseSnapshot)
	}{
		{"缺少费用 ID", func(s *events.ExpenseSnapshot) { s.ID = uuid.Nil }},
		{"缺少提交人", func(s *events.ExpenseSnapshot) { s.SubmitterID = uuid.Nil }},
		{"非法状态", func(s *events.ExpenseSnapshot) { s.Status = "paid" }},
		{"非法类别", func(s *events.ExpenseSnapshot) { s.Category = "gift" }},
		{"币种小写", func(s *events.ExpenseSnapshot) { s.Currency = "usd" }},
		{"币种长度错误", func(s *events.ExpenseSnapshot) { s.Currency = "US" }},
		{"金额为零", func(s *events.ExpenseSnapshot) { s.AmountMinor = 0 }},
		{"金额为负", func(s *events.ExpenseSnapshot) { s.AmountMinor = -1 }},
		{"金额超上限", func(s *events.ExpenseSnapshot) { s.AmountMinor = 1_000_000_000_001 }},
		{"缺少发生时间", func(s *events.ExpenseSnapshot) { s.OccurredAt = time.Time{} }},
		{"缺少更新时间", func(s *events.ExpenseSnapshot) { s.UpdatedAt = time.Time{} }},
		{"版本为 0", func(s *events.ExpenseSnapshot) { s.Version = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := validSnapshot()
			tc.mutate(&snap)
			require.Error(t, snap.Validate())
		})
	}
}
