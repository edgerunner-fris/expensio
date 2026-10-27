package identity

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 管理视图用例：成员列表、角色调整、审计查询。
// 单独成文件，是为了让"谁能看什么、谁能改谁"集中在一处，评审时一次看完。

func (s *Service) ListMembers(ctx context.Context, q httpx.PageQuery) (httpx.Page[Member], error) {
	q.Normalize()

	var (
		members []Member
		total   int
	)
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		members, total, err = s.repo.ListMembers(ctx, tx, q.PageSize, q.Offset())
		return err
	})
	if err != nil {
		return httpx.Page[Member]{}, err
	}
	return httpx.NewPage(members, int64(total), q), nil
}

// UpdateMemberRole 的规则集中在这里，不散落到 handler：
//   - 不能改自己的角色（否则 owner 一次误操作就把自己锁在门外）；
//   - 只有 owner 能授予或修改 owner；
//   - 修改必须发生在同一租户内，由 RLS 保证，越权目标会表现为"成员不存在"。
func (s *Service) UpdateMemberRole(ctx context.Context, targetID uuid.UUID, newRole tenantctx.Role, meta RequestMeta) (*Member, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}
	if !newRole.Valid() {
		return nil, apperr.InvalidArgument("角色必须是 owner / admin / member / viewer 之一")
	}
	if targetID == uuid.Nil {
		return nil, apperr.InvalidArgument("用户 ID 非法")
	}
	if targetID == actor.UserID {
		return nil, apperr.Conflict("member.self_change", "不能修改自己的角色")
	}
	if newRole == tenantctx.RoleOwner && actor.Role != tenantctx.RoleOwner {
		return nil, apperr.Forbidden("只有 owner 可以授予 owner 角色")
	}

	var updated *Member
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		current, err := s.repo.MemberByID(ctx, tx, targetID)
		if IsNoRows(err) {
			return apperr.NotFound("成员")
		}
		if err != nil {
			return err
		}
		if current.Role == string(tenantctx.RoleOwner) && actor.Role != tenantctx.RoleOwner {
			return apperr.Forbidden("只有 owner 可以修改 owner 的角色")
		}
		if current.Role == string(newRole) {
			updated = current
			return nil
		}
		if _, err := s.repo.UpdateMemberRole(ctx, tx, targetID, string(newRole), actor.UserID); err != nil {
			return err
		}
		current.Role = string(newRole)
		updated = current
		return s.audit(ctx, tx, actor.ID, &actor.UserID, "member.role_updated", "member", targetID.String(), meta,
			map[string]any{"to": string(newRole)})
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// TenantContacts 供 notification 服务解析某租户的收件人。
//
// 租户 ID 来自服务间令牌的签名载荷（见 serviceauth），不是请求参数；
// 调用方必须先通过内部令牌校验。返回值只有通知所需的联系方式与角色。
func (s *Service) TenantContacts(ctx context.Context, tenantID uuid.UUID) ([]Member, error) {
	if tenantID == uuid.Nil {
		return nil, apperr.InvalidArgument("租户 ID 非法")
	}
	var contacts []Member
	err := s.database.WithinTenantID(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		contacts, err = s.repo.ListContacts(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return contacts, nil
}

func (s *Service) ListAuditLogs(ctx context.Context, q httpx.PageQuery) (httpx.Page[AuditEntry], error) {
	q.Normalize()

	var (
		entries []AuditEntry
		total   int
	)
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entries, total, err = s.repo.ListAudit(ctx, tx, q.PageSize, q.Offset())
		return err
	})
	if err != nil {
		return httpx.Page[AuditEntry]{}, err
	}
	return httpx.NewPage(entries, int64(total), q), nil
}
