// 事件 → 通知文案的渲染。
//
// 这里不做任何业务判断，只回答"这条事件该告诉谁、说什么"。
// 收件人来自 identity 的成员列表；通知与邮件都以事件 ID 作为幂等键，
// 同一事件无论重放多少次，收件人只会看到一条。
package notification

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
)

var categoryLabels = map[string]string{
	"travel":   "差旅",
	"meals":    "餐饮",
	"office":   "办公",
	"software": "软件",
	"other":    "其他",
}

func categoryLabel(c string) string {
	if label, ok := categoryLabels[c]; ok {
		return label
	}
	return c
}

// expenseStatusByType 与事件的业务含义绑定：类型与快照状态不一致说明生产者
// 有 bug 或消息被篡改，属于永久错误，应该进 DLQ 而不是重试。
var expenseStatusByType = map[events.Type]string{
	events.TypeExpenseSubmitted:  "submitted",
	events.TypeExpenseApproved:   "approved",
	events.TypeExpenseRejected:   "rejected",
	events.TypeExpenseCancelled:  "cancelled",
	events.TypeExpenseReimbursed: "reimbursed",
}

func decodeExpenseEvent(env events.Envelope) (events.ExpenseSnapshot, error) {
	var snap events.ExpenseSnapshot
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		return snap, fmt.Errorf("解码费用快照失败: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return snap, fmt.Errorf("费用快照非法: %w", err)
	}
	if snap.TenantID != env.TenantID {
		return snap, fmt.Errorf("快照租户 %s 与信封租户 %s 不一致", snap.TenantID, env.TenantID)
	}
	if snap.ID != env.AggregateID || snap.Version != env.AggregateVersion {
		return snap, fmt.Errorf("费用快照与信封的聚合/版本不一致")
	}
	want, ok := expenseStatusByType[env.Type]
	if !ok {
		return snap, fmt.Errorf("事件类型 %q 不属于费用主题", env.Type)
	}
	if snap.Status != want {
		return snap, fmt.Errorf("事件类型 %s 要求状态 %s，快照却是 %s", env.Type, want, snap.Status)
	}
	return snap, nil
}

func decodeSubscriptionEvent(env events.Envelope) (events.SubscriptionSnapshot, error) {
	var snap events.SubscriptionSnapshot
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		return snap, fmt.Errorf("解码订阅快照失败: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return snap, fmt.Errorf("订阅快照非法: %w", err)
	}
	if snap.TenantID != env.TenantID || snap.ID != env.AggregateID || snap.Version != env.AggregateVersion {
		return snap, fmt.Errorf("订阅快照与信封不一致")
	}
	switch env.Type {
	case events.TypeSubscriptionCreated:
		if snap.Status != "active" && snap.Status != "past_due" {
			return snap, fmt.Errorf("开通事件的订阅状态为 %q", snap.Status)
		}
	case events.TypeSubscriptionCanceled:
		if snap.Status != "canceled" {
			return snap, fmt.Errorf("取消事件的订阅状态为 %q", snap.Status)
		}
	default:
		return snap, fmt.Errorf("事件类型 %q 不属于订阅事件", env.Type)
	}
	return snap, nil
}

func decodeInvoiceEvent(env events.Envelope) (events.InvoiceSnapshot, error) {
	var snap events.InvoiceSnapshot
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		return snap, fmt.Errorf("解码账单快照失败: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return snap, fmt.Errorf("账单快照非法: %w", err)
	}
	if snap.TenantID != env.TenantID || snap.ID != env.AggregateID || snap.Version != env.AggregateVersion {
		return snap, fmt.Errorf("账单快照与信封不一致")
	}
	switch env.Type {
	case events.TypeInvoiceCreated, events.TypeInvoicePaymentFailed:
		if snap.Status != "open" {
			return snap, fmt.Errorf("事件 %s 要求账单未支付，快照状态为 %q", env.Type, snap.Status)
		}
	case events.TypeInvoicePaid:
		if snap.Status != "paid" {
			return snap, fmt.Errorf("支付成功事件要求账单已支付，快照状态为 %q", snap.Status)
		}
	default:
		return snap, fmt.Errorf("事件类型 %q 不属于账单事件", env.Type)
	}
	return snap, nil
}

// draftsForExpense 决定费用事件的收件人。
//
//   - 提交：通知其他管理者（审批人）；提交者自己不需要"待审批"提醒。
//   - 通过/驳回/报销：通知提交者。
//   - 取消：通知提交者；若提交者已不在租户内，退回给管理者。
func draftsForExpense(env events.Envelope, snap events.ExpenseSnapshot, contacts []Contact, email bool) []draft {
	amount := money.Format(snap.AmountMinor, money.Currency(snap.Currency))
	category := categoryLabel(snap.Category)

	var title, body string
	var recipients []Contact

	switch env.Type {
	case events.TypeExpenseSubmitted:
		title = "新的费用待审批"
		body = fmt.Sprintf("有一笔%s费用 %s 已提交，等待审批。", category, amount)
		recipients = managersExcept(contacts, snap.SubmitterID)
	case events.TypeExpenseApproved:
		title = "费用已通过审批"
		body = fmt.Sprintf("你提交的%s费用 %s 已通过审批。", category, amount)
		recipients = contactByID(contacts, snap.SubmitterID)
	case events.TypeExpenseRejected:
		title = "费用被驳回"
		body = fmt.Sprintf("你提交的%s费用 %s 被驳回，请修改后重新提交。", category, amount)
		recipients = contactByID(contacts, snap.SubmitterID)
	case events.TypeExpenseCancelled:
		title = "费用已取消"
		body = fmt.Sprintf("%s费用 %s 已取消。", category, amount)
		recipients = contactByID(contacts, snap.SubmitterID)
		if len(recipients) == 0 {
			recipients = managersExcept(contacts, snap.SubmitterID)
		}
	case events.TypeExpenseReimbursed:
		title = "费用已报销"
		body = fmt.Sprintf("你提交的%s费用 %s 已标记为已报销。", category, amount)
		recipients = contactByID(contacts, snap.SubmitterID)
	}

	return buildDrafts(env, recipients, title, body, "expense", snap.ID, email)
}

func draftsForSubscription(env events.Envelope, snap events.SubscriptionSnapshot, contacts []Contact, email bool) []draft {
	var title, body string
	switch env.Type {
	case events.TypeSubscriptionCreated:
		title = "订阅已开通"
		body = fmt.Sprintf("套餐 %s 已开通，共 %d 个席位，当前周期至 %s。",
			snap.PlanCode, snap.Seats, snap.CurrentPeriodEnd.Format(time.DateOnly))
	case events.TypeSubscriptionCanceled:
		title = "订阅已取消"
		body = fmt.Sprintf("套餐 %s 已取消，服务可用至 %s。",
			snap.PlanCode, snap.CurrentPeriodEnd.Format(time.DateOnly))
	}
	return buildDrafts(env, managers(contacts), title, body, "subscription", snap.ID, email)
}

func draftsForInvoice(env events.Envelope, snap events.InvoiceSnapshot, contacts []Contact, email bool) []draft {
	amount := money.Format(snap.AmountMinor, money.Currency(snap.Currency))
	var title, body string
	switch env.Type {
	case events.TypeInvoiceCreated:
		title = "新账单已生成"
		body = fmt.Sprintf("账单 %s，金额 %s，到期日 %s。", snap.Number, amount, snap.DueOn.Format(time.DateOnly))
	case events.TypeInvoicePaid:
		title = "账单已支付"
		body = fmt.Sprintf("账单 %s，金额 %s，已支付成功。", snap.Number, amount)
	case events.TypeInvoicePaymentFailed:
		title = "账单支付失败"
		body = fmt.Sprintf("账单 %s，金额 %s 支付失败，请检查支付方式后重试。", snap.Number, amount)
	}
	return buildDrafts(env, managers(contacts), title, body, "invoice", snap.ID, email)
}

func buildDrafts(env events.Envelope, recipients []Contact, title, body, resource string, resourceID uuid.UUID, email bool) []draft {
	out := make([]draft, 0, len(recipients))
	for _, c := range recipients {
		d := draft{
			Recipient:  c,
			Kind:       string(env.Type),
			Title:      title,
			Body:       body,
			Resource:   resource,
			ResourceID: resourceID,
		}
		if email {
			d.Email = Outbound{
				Recipient: c.Email,
				Subject:   "[Expensio] " + title,
				Body: fmt.Sprintf("%s\n\n%s\n\n资源编号：%s\n事件编号：%s\n",
					title, body, resourceID, env.EventID),
				DedupeKey: fmt.Sprintf("%s:%s:%s", env.Type, env.EventID, c.UserID),
			}
		}
		out = append(out, d)
	}
	return out
}

func managers(contacts []Contact) []Contact {
	out := make([]Contact, 0, len(contacts))
	for _, c := range contacts {
		if c.IsManager() {
			out = append(out, c)
		}
	}
	return out
}

func managersExcept(contacts []Contact, excluded uuid.UUID) []Contact {
	out := make([]Contact, 0, len(contacts))
	for _, c := range contacts {
		if c.IsManager() && c.UserID != excluded {
			out = append(out, c)
		}
	}
	return out
}

func contactByID(contacts []Contact, id uuid.UUID) []Contact {
	for _, c := range contacts {
		if c.UserID == id {
			return []Contact{c}
		}
	}
	return nil
}
