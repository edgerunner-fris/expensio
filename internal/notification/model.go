// Package notification 消费费用与账单事件，生成站内通知并通过邮件外发。
//
// 它不拥有任何业务事实：通知可以全部丢弃后由事件主题重建。
// 幂等键落在 processed_events；邮件先写 outbox，再由独立 worker 投递，
// 与 ADR-0004 的事务性 outbox 是同一思路。
package notification

import (
	"time"

	"github.com/google/uuid"
)

// Notification 是一条站内通知。
type Notification struct {
	ID           uuid.UUID  `json:"id"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Body         string     `json:"body"`
	ResourceType string     `json:"resource_type,omitempty"`
	ResourceID   *uuid.UUID `json:"resource_id,omitempty"`
	ReadAt       *time.Time `json:"read_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Contact 是 identity 内部接口返回的收件人信息。
type Contact struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
}

// IsManager 判断联系人是否能管理账单/审批费用。
func (c Contact) IsManager() bool { return c.Role == "owner" || c.Role == "admin" }

// Outbound 是一封待投递邮件。DedupeKey 在租户内唯一，重复事件不会重复发信。
type Outbound struct {
	Recipient string
	Subject   string
	Body      string
	DedupeKey string
}

// draft 是"某条通知 + 对应邮件"的中间形态，由事件渲染产生。
type draft struct {
	Recipient  Contact
	Kind       string
	Title      string
	Body       string
	Resource   string
	ResourceID uuid.UUID
	Email      Outbound
}
