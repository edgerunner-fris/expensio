package identity

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// DeleteTenantInput 用 slug 做二次确认。
//
// 删除租户不可撤销，且影响该租户下的所有成员；只凭一枚 access token
// 就能执行太容易误触（尤其是脚本或前端 bug）。要求调用方把租户标识
// 原样写回来，与"输入 DELETE 确认"是同一个思路。
type DeleteTenantInput struct {
	Slug string `json:"slug"`
}

// DeleteTenantResult 返回被撤销的会话数，供前端提示与审计核对。
type DeleteTenantResult struct {
	SessionsRevoked int64 `json:"sessions_revoked"`
}

// DeleteTenant 软删除当前租户：租户行与全部成员关系打上 deleted_at，
// 该租户的刷新令牌全部撤销，未接受的邀请全部作废。
//
// 这里刻意不做物理删除：
//   - 其他服务（费用、账单、报表、附件）的租户数据仍以 tenant_id 为键，
//     物理清理需要按服务逐个执行，不能由 identity 一个事务跨库完成；
//   - 财务与审计记录有保留要求，删除应当是"不可访问"而不是"立即消失"。
//
// 软删除后：登录与租户切换查不到该租户；刷新令牌已撤销；已签发的 access
// token 在剩余 TTL（≤15 分钟）内仍可能被各服务接受，这与改密后的行为一致，
// 是"无状态 JWT + 无逐请求黑名单"的直接代价，见 ADR-0014。
func (s *Service) DeleteTenant(ctx context.Context, in DeleteTenantInput, meta RequestMeta) (*DeleteTenantResult, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}
	// 路由层已经用 RequireRole 挡过一次；服务层再判一次，避免将来有
	// 新的调用点绕过中间件。所有者身份来自 JWT，不接受请求参数。
	if actor.Role != tenantctx.RoleOwner {
		return nil, apperr.Forbidden("只有所有者可以删除租户")
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return nil, apperr.InvalidArgument("请输入租户标识以确认删除")
	}

	result := &DeleteTenantResult{}
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tenant, err := s.repo.TenantByID(ctx, tx, actor.ID)
		if err != nil {
			if IsNoRows(err) {
				return apperr.NotFound("租户")
			}
			return err
		}
		if tenant.Slug != slug {
			return apperr.InvalidArgument("租户标识不匹配，未执行删除")
		}

		// 审计先写：它是该租户的最后一条操作记录。审计行不随租户软删除消失，
		// 保留期内仍可由运维按租户 ID 检索。
		if err := s.audit(ctx, tx, actor.ID, &actor.UserID, "tenant.deleted",
			"tenant", actor.ID.String(), meta,
			map[string]any{"slug": tenant.Slug}); err != nil {
			return err
		}

		revoked, err := s.repo.RevokeTenantRefreshTokens(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		result.SessionsRevoked = revoked

		if _, err := s.repo.RevokeTenantInvitations(ctx, tx, actor.ID); err != nil {
			return err
		}
		if _, err := s.repo.SoftDeleteTenantMemberships(ctx, tx, actor.ID, actor.UserID); err != nil {
			return err
		}
		deleted, err := s.repo.SoftDeleteTenant(ctx, tx, actor.ID, actor.UserID)
		if err != nil {
			return err
		}
		if !deleted {
			// 并发删除：另一个请求已经先一步完成。整体回滚，返回冲突而不是
			// 假装成功（刷新令牌与审计已经在本事务里写过，回滚一起撤销）。
			return apperr.Conflict("tenant.already_deleted", "租户已被删除")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
