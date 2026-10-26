// Package tenantctx 在全链路上传递租户上下文。
//
// 租户 ID 的唯一合法来源是校验通过的 access token 中的 claim。
// 本包不提供"从请求参数构造上下文"的入口，也不提供修改已有上下文的入口——
// 任何从 body / query / path 读 tenant_id 的代码都走不到这里。
package tenantctx

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Role 是租户内的角色，权限点由角色映射而来。
// 取值与数据库 CHECK 约束、JWT claim 保持一致的小写形式，
// 避免在每一层都做一次大小写转换——转换点越多，越容易漏掉某一条路径。
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	default:
		return false
	}
}

// rank 是角色的权限强弱，数值越大权限越高。
func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	case RoleViewer:
		return 0
	default:
		return -1
	}
}

// AtLeast 判断当前角色是否不低于 min。
// 未知角色一律不通过：配置写错时应该是拒绝，而不是放行。
func (r Role) AtLeast(min Role) bool {
	if !r.Valid() {
		return false
	}
	return r.rank() >= min.rank()
}

// CanManageMembers 表示是否可以管理成员：仅 owner 与 admin。
func (r Role) CanManageMembers() bool { return r.AtLeast(RoleAdmin) }

// Tenant 是一次请求的身份视图，不含任何凭证。
type Tenant struct {
	ID     uuid.UUID
	Slug   string
	UserID uuid.UUID
	Role   Role
}

type ctxKey struct{}

// With 把租户上下文写入 ctx。ID 或用户 ID 为空时拒绝，避免"零值租户"一路传到 SQL。
func With(ctx context.Context, t Tenant) (context.Context, error) {
	if t.ID == uuid.Nil {
		return nil, errors.New("tenantctx: 租户 ID 不能为空")
	}
	if t.UserID == uuid.Nil {
		return nil, errors.New("tenantctx: 用户 ID 不能为空")
	}
	if !t.Role.Valid() {
		return nil, fmt.Errorf("tenantctx: 非法角色 %q", t.Role)
	}
	return context.WithValue(ctx, ctxKey{}, t), nil
}

// From 读取租户上下文。第二个返回值表示是否已认证。
func From(ctx context.Context) (Tenant, bool) {
	t, ok := ctx.Value(ctxKey{}).(Tenant)
	return t, ok
}

// ID 返回租户 ID，未认证时返回 uuid.Nil。
func ID(ctx context.Context) uuid.UUID {
	t, ok := From(ctx)
	if !ok {
		return uuid.Nil
	}
	return t.ID
}
