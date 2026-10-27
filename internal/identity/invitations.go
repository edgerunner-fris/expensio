package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 邀请流程：
//   管理员创建 → 邮件里带一次性链接（库里只存摘要）→ 被邀请人打开链接
//   → 设置密码（已有账号则确认密码）→ 成为 active 成员并获得会话。
//
// 权限规则与角色调整保持一致：admin 可以邀请 member/viewer/admin，
// 只有 owner 能邀请 owner；不能邀请已经是本租户成员的人。

const invitationTokenBytes = 32

type CreateInvitationInput struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type AcceptInvitationInput struct {
	Password string `json:"password"`
	// DisplayName 只用于创建新账号；已有账号忽略此字段。
	DisplayName string `json:"display_name"`
}

func (s *Service) CreateInvitation(ctx context.Context, in CreateInvitationInput, meta RequestMeta) (*CreatedInvitation, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}

	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	role := tenantctx.Role(strings.TrimSpace(in.Role))
	if !role.Valid() {
		return nil, apperr.InvalidArgument("角色必须是 owner / admin / member / viewer 之一")
	}
	if role == tenantctx.RoleOwner && actor.Role != tenantctx.RoleOwner {
		return nil, apperr.Forbidden("只有 owner 可以邀请 owner")
	}

	plain, hash, err := newInvitationToken()
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	now := time.Now().UTC()
	expiresAt := now.Add(s.inviteTTL)
	link := s.inviteBaseURL + "/invite/" + plain

	inv := &invitationRecord{
		ID: id, TenantID: actor.ID, Email: email, Role: string(role),
		ExpiresAt: expiresAt, CreatedAt: now,
	}

	var tenantName string
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tenant, err := s.repo.TenantByID(ctx, tx, actor.ID)
		if err != nil {
			return err
		}
		tenantName = tenant.Name

		member, err := s.repo.MemberByEmail(ctx, tx, email)
		switch {
		case err == nil:
			if member.Status == "active" {
				return apperr.Conflict("member.already_member", "该邮箱已是本租户成员")
			}
			return apperr.Conflict("member.disabled", "该账号在此租户中已被停用")
		case !IsNoRows(err):
			return err
		}

		// 同一邮箱重复邀请：旧链接立即失效，最新一封邮件才是有效的。
		if _, err := s.repo.RevokePendingInvitationByEmail(ctx, tx, email); err != nil {
			return err
		}
		if err := s.repo.InsertInvitation(ctx, tx, inv, hash, actor.UserID); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor.ID, &actor.UserID, "member.invited", "invitation", id.String(), meta,
			map[string]any{"email": email, "role": string(role), "expires_at": expiresAt.Format(time.RFC3339)})
	})
	if err != nil {
		return nil, err
	}

	out := &CreatedInvitation{
		Invitation: Invitation{
			ID: id, Email: email, Role: string(role), InvitedBy: actor.UserID,
			ExpiresAt: expiresAt, CreatedAt: now,
		},
		Link:        link,
		EmailQueued: false,
	}
	if s.notifier == nil {
		return out, nil
	}
	// 邀请已经落库、链接已经可用。邮件入队失败不回滚：让管理员拿到链接
	// 手动转发，比返回 500 然后让人猜"到底创建成功没有"有用。
	if err := s.notifier.EnqueueInvitation(ctx, actor.ID, InvitationEmail{
		InvitationID: id, Recipient: email, TenantName: tenantName,
		Link: link, ExpiresAt: expiresAt,
	}); err != nil {
		logger.FromContext(ctx).Warn("identity: 邀请已创建，但邮件入队失败",
			zap.String("invitation_id", id.String()), zap.Error(err))
		return out, nil
	}
	out.EmailQueued = true
	return out, nil
}

func (s *Service) ListInvitations(ctx context.Context, q httpx.PageQuery) (httpx.Page[Invitation], error) {
	q.Normalize()

	var (
		items []Invitation
		total int
	)
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, total, err = s.repo.ListInvitations(ctx, tx, q.PageSize, q.Offset())
		return err
	})
	if err != nil {
		return httpx.Page[Invitation]{}, err
	}
	return httpx.NewPage(items, int64(total), q), nil
}

func (s *Service) RevokeInvitation(ctx context.Context, id uuid.UUID, meta RequestMeta) error {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return apperr.Unauthorized("缺少访问令牌")
	}
	if id == uuid.Nil {
		return apperr.InvalidArgument("邀请 ID 非法")
	}
	return s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		revoked, err := s.repo.RevokeInvitation(ctx, tx, id)
		if err != nil {
			return err
		}
		if !revoked {
			if _, err := s.repo.InvitationByID(ctx, tx, id); IsNoRows(err) {
				return apperr.NotFound("邀请")
			} else if err != nil {
				return err
			}
			return apperr.Conflict("invitation.not_pending", "该邀请已被接受或撤销")
		}
		return s.audit(ctx, tx, actor.ID, &actor.UserID, "member.invitation_revoked",
			"invitation", id.String(), meta, nil)
	})
}

// InvitationInfo 是公开接口：令牌本身就是要展示的凭据。
// 它只返回接受页需要的信息，不返回邀请人、历史或任何其它邮箱。
func (s *Service) InvitationInfo(ctx context.Context, token string) (*InvitationInfo, error) {
	inv, err := s.findInvitation(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := invitationStateError(inv, time.Now()); err != nil {
		return nil, err
	}

	var exists bool
	err = s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.repo.UserByEmail(ctx, tx, inv.Email)
		switch {
		case err == nil:
			exists = true
			return nil
		case IsNoRows(err):
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}
	return &InvitationInfo{
		TenantName: inv.TenantName, TenantSlug: inv.TenantSlug,
		Email: inv.Email, Role: inv.Role, ExpiresAt: inv.ExpiresAt,
		AccountExists: exists,
	}, nil
}

// AcceptInvitation 完成加入流程。密码总是必需：
//   - 新账号：密码是注册凭据，DisplayName 可选；
//   - 已有账号：密码用来确认"链接持有者 == 账号主人"，
//     避免有人转发邮件链接就能接管别人的账号。
func (s *Service) AcceptInvitation(ctx context.Context, token string, in AcceptInvitationInput, meta RequestMeta) (*Session, error) {
	inv, err := s.findInvitation(ctx, token)
	if err != nil {
		return nil, err
	}
	if err := invitationStateError(inv, time.Now()); err != nil {
		return nil, err
	}
	if in.Password == "" {
		return nil, apperr.InvalidArgument("请输入密码")
	}

	var existing *User
	err = s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		u, err := s.repo.UserByEmail(ctx, tx, inv.Email)
		switch {
		case err == nil:
			existing = u
			return nil
		case IsNoRows(err):
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}

	var (
		displayName  string
		passwordHash string
	)
	if existing != nil {
		if existing.Status != "active" {
			return nil, apperr.Forbidden("该账号已被停用")
		}
		ok, verifyErr := auth.VerifyPassword(existing.PasswordHash, in.Password)
		if verifyErr != nil {
			logger.FromContext(ctx).Error("identity: 存储的密码哈希非法",
				zap.String("user_id", existing.ID.String()), zap.Error(verifyErr))
			return nil, errInvalidCredentials()
		}
		if !ok {
			return nil, errInvalidCredentials()
		}
	} else {
		if err := validatePassword(in.Password); err != nil {
			return nil, err
		}
		displayName = strings.TrimSpace(in.DisplayName)
		if displayName == "" {
			displayName = inv.Email[:strings.IndexByte(inv.Email, '@')]
		}
		if utf8.RuneCountInString(displayName) > maxDisplayLen {
			return nil, apperr.InvalidArgument("显示名称不超过 64 个字符")
		}
		// 哈希在事务外计算：argon2 需要几十毫秒，不该占着数据库连接。
		passwordHash, err = auth.HashPassword(in.Password)
		if err != nil {
			return nil, apperr.Internal(err)
		}
	}

	var session *Session
	err = s.database.WithinTenantID(ctx, inv.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// 行锁把并发接受串行化：第二个请求会在这里发现邀请已不再 pending。
		pending, err := s.repo.LockPendingInvitation(ctx, tx, inv.ID)
		if err != nil {
			return err
		}
		if !pending {
			return apperr.New("invitation.not_pending", http.StatusGone, "邀请已被使用、撤销或过期")
		}

		var userID uuid.UUID
		if existing != nil {
			userID = existing.ID
			m, err := s.repo.Membership(ctx, tx, userID)
			switch {
			case IsNoRows(err):
				if err := s.repo.CreateMembership(ctx, tx, inv.TenantID, userID, inv.Role, "active", userID); err != nil {
					return err
				}
			case err != nil:
				return err
			case m.Status == "disabled":
				return apperr.Forbidden("该账号在此租户中已被停用")
			case m.Status == "invited":
				if _, err := s.repo.ActivateMembership(ctx, tx, userID, inv.Role, userID); err != nil {
					return err
				}
			}
		} else {
			userID = uuid.New()
			if err := s.repo.CreateUser(ctx, tx, &User{
				ID: userID, Email: inv.Email, PasswordHash: passwordHash,
				DisplayName: displayName, Status: "active",
			}); err != nil {
				if UniqueConstraint(err) == "users_email_lower_key" {
					return apperr.Conflict("auth.email_taken", "该邮箱刚被注册，请重新打开邀请链接")
				}
				return err
			}
			if err := s.repo.CreateMembership(ctx, tx, inv.TenantID, userID, inv.Role, "active", userID); err != nil {
				return err
			}
		}

		sub, err := s.subjectByID(ctx, tx, inv.TenantID, userID)
		if err != nil {
			return err
		}
		if session, err = s.issueSession(ctx, tx, sub, uuid.Nil, meta); err != nil {
			return err
		}
		if _, err := s.repo.AcceptInvitation(ctx, tx, inv.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, inv.TenantID, &userID, "member.invitation_accepted",
			"member", userID.String(), meta, map[string]any{"role": sub.Role})
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Service) findInvitation(ctx context.Context, token string) (*invitationRecord, error) {
	hash, err := invitationTokenHash(token)
	if err != nil {
		return nil, err
	}
	var inv *invitationRecord
	err = s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := s.repo.FindInvitationByHash(ctx, tx, hash)
		if IsNoRows(err) {
			return apperr.NotFound("邀请")
		}
		if err != nil {
			return err
		}
		inv = rec
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// invitationStateError 把非 pending 的邀请统一映射成 410 Gone：
// 链接是有效的（确实存在），但已经不能再用了。
func invitationStateError(inv *invitationRecord, now time.Time) error {
	switch {
	case inv.AcceptedAt != nil:
		return apperr.New("invitation.accepted", http.StatusGone, "邀请已被使用")
	case inv.RevokedAt != nil:
		return apperr.New("invitation.revoked", http.StatusGone, "邀请已被撤销")
	case !now.Before(inv.ExpiresAt):
		return apperr.New("invitation.expired", http.StatusGone, "邀请已过期")
	default:
		return nil
	}
}

// newInvitationToken 返回明文与摘要。明文只出现在响应和邮件里，
// 数据库只保存 sha256 摘要。
func newInvitationToken() (string, []byte, error) {
	raw := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, apperr.Internal(fmt.Errorf("identity: 生成邀请令牌失败: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw), hashInvitation(raw), nil
}

// invitationTokenHash 把 URL 里的令牌还原成摘要。
// 长度或编码不合法时一律按"邀请不存在"处理，不区分原因。
func invitationTokenHash(token string) ([]byte, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 128 {
		return nil, apperr.NotFound("邀请")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != invitationTokenBytes {
		return nil, apperr.NotFound("邀请")
	}
	return hashInvitation(raw), nil
}

func hashInvitation(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}
