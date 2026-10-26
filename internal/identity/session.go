package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 会话签发与主体加载：登录、刷新、me 共用这一段。
// 令牌只在这里产生，其他代码拿到的都是已签好的字符串。

// sessionSubject 是签发会话所需的展示信息，来自数据库而不是客户端输入。
type sessionSubject struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
	TenantID    uuid.UUID
	TenantSlug  string
	TenantName  string
	Role        tenantctx.Role
}

// issueSession 新建一条刷新令牌链。
func (s *Service) issueSession(ctx context.Context, tx pgx.Tx, sub sessionSubject, familyID uuid.UUID, meta RequestMeta) (*Session, error) {
	return s.issueSessionWithID(ctx, tx, sub, familyID, uuid.New(), meta)
}

// issueSessionWithID 允许调用方指定刷新令牌主键，供轮换流程复用已经写入的 replaced_by。
func (s *Service) issueSessionWithID(ctx context.Context, tx pgx.Tx, sub sessionSubject, familyID, refreshID uuid.UUID, meta RequestMeta) (*Session, error) {
	if familyID == uuid.Nil {
		familyID = uuid.New()
	}
	plain, hash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, apperr.Internal(err)
	}
	// 对外统一用 UTC：服务可能部署在不同时区，客户端不该从时间字符串里推断服务器位置。
	refreshExpiresAt := time.Now().UTC().Add(s.tokens.RefreshTTL())
	if err := s.repo.CreateRefreshToken(ctx, tx, refreshID, sub.TenantID, sub.UserID, familyID, hash, refreshExpiresAt, meta); err != nil {
		return nil, err
	}

	// sid 用令牌链 ID：登出一次即可让同一会话派生出的所有令牌失效。
	access, accessExpiresAt, err := s.tokens.IssueAccess(sub.UserID, sub.TenantID, sub.Role, familyID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	accessExpiresAt = accessExpiresAt.UTC()

	return &Session{
		AccessToken:      access,
		AccessExpiresAt:  accessExpiresAt,
		RefreshToken:     plain,
		RefreshExpiresAt: refreshExpiresAt,
		Profile: Profile{
			UserID: sub.UserID, Email: sub.Email, DisplayName: sub.DisplayName,
			TenantID: sub.TenantID, TenantSlug: sub.TenantSlug, TenantName: sub.TenantName,
			Role: string(sub.Role),
		},
	}, nil
}

// subjectByID 在租户上下文里读取用户与成员关系，顺带校验状态。
func (s *Service) subjectByID(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) (sessionSubject, error) {
	tenant, err := s.repo.TenantByID(ctx, tx, tenantID)
	if IsNoRows(err) {
		return sessionSubject{}, apperr.Unauthorized("登录状态已失效，请重新登录")
	}
	if err != nil {
		return sessionSubject{}, err
	}
	user, err := s.repo.UserByID(ctx, tx, userID)
	if IsNoRows(err) {
		return sessionSubject{}, apperr.Unauthorized("登录状态已失效，请重新登录")
	}
	if err != nil {
		return sessionSubject{}, err
	}
	m, err := s.repo.Membership(ctx, tx, userID)
	if IsNoRows(err) {
		return sessionSubject{}, apperr.Forbidden("该账号不属于此租户")
	}
	if err != nil {
		return sessionSubject{}, err
	}
	if user.Status != "active" {
		return sessionSubject{}, apperr.Forbidden("该账号已被停用")
	}
	if m.Status != "active" {
		return sessionSubject{}, apperr.Forbidden("该账号在此租户中已被停用")
	}
	return sessionSubject{
		UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName,
		TenantID: tenant.ID, TenantSlug: tenant.Slug, TenantName: tenant.Name,
		Role: tenantctx.Role(m.Role),
	}, nil
}
