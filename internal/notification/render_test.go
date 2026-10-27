package notification

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
)

var renderClock = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

func mustEncode(t *testing.T, env events.Envelope) []byte {
	t.Helper()
	raw, err := env.Encode()
	require.NoError(t, err)
	return raw
}

func expenseEnvelope(t *testing.T, typ events.Type, status string, version int, amount int64) events.Envelope {
	t.Helper()
	tenantID, expenseID, submitterID := uuid.New(), uuid.New(), uuid.New()
	snap := events.ExpenseSnapshot{
		ID:          expenseID,
		TenantID:    tenantID,
		SubmitterID: submitterID,
		Status:      status,
		Category:    "travel",
		Currency:    "USD",
		AmountMinor: amount,
		OccurredAt:  renderClock.Add(-24 * time.Hour),
		Version:     version,
		CreatedAt:   renderClock.Add(-24 * time.Hour),
		UpdatedAt:   renderClock,
	}
	payload, err := json.Marshal(snap)
	require.NoError(t, err)
	return events.Envelope{
		EventID:          uuid.New(),
		Type:             typ,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         tenantID,
		AggregateID:      expenseID,
		AggregateVersion: version,
		OccurredAt:       renderClock,
		Payload:          payload,
	}
}

func TestDraftsForSubmittedGoToOtherManagers(t *testing.T) {
	env := expenseEnvelope(t, events.TypeExpenseSubmitted, "submitted", 1, 10000)
	snap, err := decodeExpenseEvent(env)
	require.NoError(t, err)

	owner := Contact{UserID: uuid.New(), Email: "owner@example.com", Role: "owner"}
	submitter := Contact{UserID: snap.SubmitterID, Email: "admin@example.com", Role: "admin"}
	member := Contact{UserID: uuid.New(), Email: "member@example.com", Role: "member"}

	drafts := draftsForExpense(env, snap, []Contact{owner, submitter, member}, true)
	require.Len(t, drafts, 1)
	assert.Equal(t, owner.UserID, drafts[0].Recipient.UserID, "提交者自己不应收到待审批提醒")
	assert.Equal(t, "expense", drafts[0].Resource)
	assert.Equal(t, snap.ID, drafts[0].ResourceID)
	assert.Contains(t, drafts[0].Body, "100.00 USD")
	assert.Contains(t, drafts[0].Body, "差旅")
	assert.Equal(t, "[Expensio] 新的费用待审批", drafts[0].Email.Subject)
	assert.Contains(t, drafts[0].Email.DedupeKey, env.EventID.String())
}

func TestDraftsForDecisionGoToSubmitter(t *testing.T) {
	env := expenseEnvelope(t, events.TypeExpenseApproved, "approved", 2, 2500)
	snap, err := decodeExpenseEvent(env)
	require.NoError(t, err)

	owner := Contact{UserID: uuid.New(), Email: "owner@example.com", Role: "owner"}
	submitter := Contact{UserID: snap.SubmitterID, Email: "member@example.com", Role: "member"}

	drafts := draftsForExpense(env, snap, []Contact{owner, submitter}, true)
	require.Len(t, drafts, 1)
	assert.Equal(t, submitter.UserID, drafts[0].Recipient.UserID)
	assert.Contains(t, drafts[0].Body, "25.00 USD")
	assert.Contains(t, drafts[0].Body, "已通过审批")
}

func TestDraftsSkipEmailWhenDisabled(t *testing.T) {
	env := expenseEnvelope(t, events.TypeExpenseRejected, "rejected", 2, 100)
	snap, err := decodeExpenseEvent(env)
	require.NoError(t, err)
	submitter := Contact{UserID: snap.SubmitterID, Email: "member@example.com", Role: "member"}

	drafts := draftsForExpense(env, snap, []Contact{submitter}, false)
	require.Len(t, drafts, 1)
	assert.Empty(t, drafts[0].Email.DedupeKey)
}

func TestDecodeExpenseEventRejectsTypeStatusMismatch(t *testing.T) {
	env := expenseEnvelope(t, events.TypeExpenseApproved, "submitted", 2, 100)
	_, err := decodeExpenseEvent(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "要求状态 approved")
}

func TestDecodeInvoiceEventRejectsPaidSnapshotOnFailureEvent(t *testing.T) {
	tenantID, invoiceID := uuid.New(), uuid.New()
	snap := events.InvoiceSnapshot{
		ID:             invoiceID,
		TenantID:       tenantID,
		SubscriptionID: uuid.New(),
		Number:         "INV-2026-0001",
		Status:         "paid",
		PeriodStart:    renderClock.Add(-30 * 24 * time.Hour),
		PeriodEnd:      renderClock,
		Currency:       "USD",
		AmountMinor:    123450,
		DueOn:          renderClock,
		IssuedAt:       renderClock.Add(-24 * time.Hour),
		PaidAt:         &renderClock,
		Version:        2,
	}
	payload, err := json.Marshal(snap)
	require.NoError(t, err)
	env := events.Envelope{
		EventID:          uuid.New(),
		Type:             events.TypeInvoicePaymentFailed,
		SchemaVersion:    events.SchemaVersion,
		TenantID:         tenantID,
		AggregateID:      invoiceID,
		AggregateVersion: 2,
		OccurredAt:       renderClock,
		Payload:          payload,
	}
	_, err = decodeInvoiceEvent(env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "未支付")
}

func TestBuildMessageEncodesChineseSubject(t *testing.T) {
	msg := buildMessage("no-reply@expensio.local", "user@example.com",
		"新的费用待审批", "第一行\n第二行")
	assert.Contains(t, msg, "Subject: =?UTF-8?")
	assert.Contains(t, msg, "Content-Type: text/plain; charset=UTF-8")
	assert.Contains(t, msg, "\r\n第一行\r\n第二行\r\n")
	assert.NotContains(t, msg, "\n\n\r\n", "正文换行必须统一为 CRLF")
}

func TestEmailBackoffCapsAtOneHour(t *testing.T) {
	assert.Equal(t, time.Minute, emailBackoff(1))
	assert.Equal(t, 2*time.Minute, emailBackoff(2))
	assert.Equal(t, time.Hour, emailBackoff(20))
}
