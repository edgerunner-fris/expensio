package identity

import (
	"time"

	"github.com/google/uuid"
)

// Tenant 是租户本体。
type Tenant struct {
	ID     uuid.UUID
	Slug   string
	Name   string
	Plan   string
	Status string
}

// User 是全局账号（可属于多个租户）。
// password_hash 显式标记为不参与序列化：即使有人误把 User 塞进响应也不会泄露。
type User struct {
	ID           uuid.UUID `json:"user_id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"display_name"`
	Status       string    `json:"status"`
}

// Membership 是"某用户在某租户里的身份"。
type Membership struct {
	UserID uuid.UUID
	Role   string
	Status string
}

// Profile 是对外暴露的"当前身份"视图。
// 单独定义而不是复用 User：避免某天给 User 加字段时顺带泄露出去。
type Profile struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	TenantID    uuid.UUID `json:"tenant_id"`
	TenantSlug  string    `json:"tenant_slug"`
	TenantName  string    `json:"tenant_name"`
	Role        string    `json:"role"`
}

// Member 是成员列表的一行，含展示所需的用户信息。
type Member struct {
	UserID      uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// RefreshToken 是一行刷新令牌记录。明文不入库。
type RefreshToken struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	UserID     uuid.UUID
	FamilyID   uuid.UUID
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *uuid.UUID
}

// AuditEntry 是审计日志的一行。metadata 只放非敏感上下文。
type AuditEntry struct {
	ID          uuid.UUID      `json:"id"`
	ActorUserID *uuid.UUID     `json:"actor_user_id,omitempty"`
	Action      string         `json:"action"`
	TargetType  string         `json:"target_type,omitempty"`
	TargetID    string         `json:"target_id,omitempty"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
}

// RequestMeta 记录请求来源，用于审计与刷新令牌绑定。
// 取值来自 gin 的 ClientIP（已关闭信任代理，见 server.New）与 User-Agent 头，
// 前者在关闭信任代理后无法被 X-Forwarded-For 伪造。
type RequestMeta struct {
	IP        string
	UserAgent string
}

// AuditRecord 是待写入的一条审计事件。
// 与读出的 AuditEntry 分开：写入需要请求来源，读出要按租户过滤，
// 两边的字段本来就不同，复用同一个结构只会让序列化结果带上不该出现的字段。
type AuditRecord struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ActorUserID *uuid.UUID
	Action      string
	TargetType  string
	TargetID    string
	Meta        RequestMeta
	Metadata    map[string]any
}

// Session 是一次登录的结果。
type Session struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	Profile          Profile   `json:"profile"`
}
