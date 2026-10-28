package identity

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// TenantMembership 是"我在某个租户里的身份"，用于租户切换列表。
// status 是成员关系状态（active / disabled），tenant_status 是租户状态。
type TenantMembership struct {
	TenantID     uuid.UUID `json:"tenant_id"`
	Slug         string    `json:"slug"`
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	TenantStatus string    `json:"tenant_status"`
	Current      bool      `json:"current"`
}

// UpdateProfileInput 只允许修改展示名。邮箱是登录凭据的一部分，不在这里改。
type UpdateProfileInput struct {
	DisplayName string `json:"display_name"`
}

// ChangePasswordInput 需要当前密码：仅凭 access token 改密会让被盗令牌
// 直接变成账号接管。
type ChangePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func profileFromSubject(sub sessionSubject) *Profile {
	return &Profile{
		UserID: sub.UserID, Email: sub.Email, DisplayName: sub.DisplayName,
		TenantID: sub.TenantID, TenantSlug: sub.TenantSlug, TenantName: sub.TenantName,
		Role: string(sub.Role),
	}
}

// Tenants 列出当前账号的全部成员关系。
// memberships 是 FORCE RLS、按当前租户隔离，"我的其他租户"只能在用户上下文里读：
// db.WithinUser 设置 app.user_id，self_membership_read 策略只放行本人的行。
func (s *Service) Tenants(ctx context.Context) ([]TenantMembership, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}

	var out []TenantMembership
	err := s.database.WithinUser(ctx, actor.UserID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.UserTenants(ctx, tx, actor.UserID)
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Current = out[i].TenantID == actor.ID
	}
	return out, nil
}

// SwitchTenant 为当前账号签发目标租户的新会话。
//
// 旧的刷新令牌链保持有效："切换"不是撤销凭据，其他设备/客户端上的会话
// 不受影响，撤销仍然走登出。目标租户的一切判断都来自数据库，不接受
// 客户端的角色或状态声明。
func (s *Service) SwitchTenant(ctx context.Context, targetID uuid.UUID, meta RequestMeta) (*Session, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}
	if targetID == uuid.Nil {
		return nil, apperr.InvalidArgument("租户 ID 不能为空")
	}
	if targetID == actor.ID {
		return nil, apperr.InvalidArgument("已经在该租户中")
	}

	var target *TenantMembership
	err := s.database.WithinUser(ctx, actor.UserID, func(ctx context.Context, tx pgx.Tx) error {
		all, err := s.repo.UserTenants(ctx, tx, actor.UserID)
		if err != nil {
			return err
		}
		for i := range all {
			if all[i].TenantID == targetID {
				target = &all[i]
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 不区分"租户不存在"与"不是成员"：两者都是无权访问，也不该成为探测点。
	if target == nil {
		return nil, apperr.Forbidden("该账号不属于此租户")
	}
	if target.Status != "active" {
		return nil, apperr.Forbidden("该账号在此租户中已被停用")
	}
	if target.TenantStatus != "active" {
		return nil, apperr.Forbidden("该租户已被停用")
	}

	var session *Session
	err = s.database.WithinTenantID(ctx, targetID, func(ctx context.Context, tx pgx.Tx) error {
		// subjectByID 再读一次租户、用户与成员关系：上面的列表与本事务之间
		// 存在时间窗口，签发前的最终判定以本事务为准。
		sub, err := s.subjectByID(ctx, tx, targetID, actor.UserID)
		if err != nil {
			return err
		}
		if session, err = s.issueSession(ctx, tx, sub, uuid.Nil, meta); err != nil {
			return err
		}
		return s.audit(ctx, tx, targetID, &actor.UserID, "auth.tenant_switched", "tenant",
			targetID.String(), meta, map[string]any{"from_tenant_id": actor.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// UpdateProfile 修改展示名。账号是全局的，改名对所有租户生效。
func (s *Service) UpdateProfile(ctx context.Context, in UpdateProfileInput, meta RequestMeta) (*Profile, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}
	name, err := validateDisplayName(in.DisplayName)
	if err != nil {
		return nil, err
	}

	var profile *Profile
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		updated, err := s.repo.UpdateDisplayName(ctx, tx, actor.UserID, name)
		if err != nil {
			return err
		}
		if !updated {
			return errSessionExpired()
		}
		if err := s.audit(ctx, tx, actor.ID, &actor.UserID, "user.profile_updated",
			"user", actor.UserID.String(), meta, map[string]any{"field": "display_name"}); err != nil {
			return err
		}
		sub, err := s.subjectByID(ctx, tx, actor.ID, actor.UserID)
		if err != nil {
			return err
		}
		profile = profileFromSubject(sub)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// ChangePassword 校验当前密码后更新哈希，并撤销该账号的全部刷新令牌链。
//
// 分两个事务：第一个只读并校验（argon2 不占着事务），第二个用
// password_hash 做条件更新（CAS），避免与另一台设备同时改密时互相覆盖。
func (s *Service) ChangePassword(ctx context.Context, in ChangePasswordInput, meta RequestMeta) error {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return apperr.Unauthorized("缺少访问令牌")
	}
	if in.CurrentPassword == "" {
		return apperr.InvalidArgument("请输入当前密码")
	}
	if err := validatePassword(in.NewPassword); err != nil {
		return err
	}

	var currentHash string
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		user, err := s.repo.UserByID(ctx, tx, actor.UserID)
		if IsNoRows(err) {
			return errSessionExpired()
		}
		if err != nil {
			return err
		}
		currentHash = user.PasswordHash
		return nil
	})
	if err != nil {
		return err
	}

	okCurrent, err := auth.VerifyPassword(currentHash, in.CurrentPassword)
	if err != nil {
		logger.FromContext(ctx).Error("identity: 存储的密码哈希非法",
			zap.String("user_id", actor.UserID.String()), zap.Error(err))
		return errInvalidCredentials()
	}
	if !okCurrent {
		return apperr.Unauthorized("当前密码不正确")
	}
	same, err := auth.VerifyPassword(currentHash, in.NewPassword)
	if err != nil {
		logger.FromContext(ctx).Error("identity: 存储的密码哈希非法",
			zap.String("user_id", actor.UserID.String()), zap.Error(err))
		return errInvalidCredentials()
	}
	if same {
		return apperr.InvalidArgument("新密码不能与当前密码相同")
	}

	newHash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		return apperr.Internal(err)
	}

	return s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		updated, err := s.repo.UpdatePassword(ctx, tx, actor.UserID, currentHash, newHash)
		if err != nil {
			return err
		}
		if !updated {
			// 两次事务之间另一台设备改了密码；此时撤销旧链由对方完成。
			return apperr.Conflict("auth.password_changed", "密码已被其他会话修改，请重新登录")
		}
		revoked, err := s.repo.RevokeAllRefreshTokens(ctx, tx, actor.UserID)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, actor.ID, &actor.UserID, "auth.password_changed",
			"user", actor.UserID.String(), meta, map[string]any{"sessions_revoked": revoked})
	})
}
