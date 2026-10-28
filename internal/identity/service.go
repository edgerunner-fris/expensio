package identity

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/logger"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	minPasswordLen = 10
	// 上限防止有人用 1MB 的"密码"把 argon2 变成 CPU 放大器。
	maxPasswordLen = 128
	maxEmailLen    = 254
	maxNameLen     = 80
	maxDisplayLen  = 64
)

// slugPattern 与数据库 CHECK 约束保持同一套规则：应用层先给出友好错误，
// 约束负责在并发与旁路写入时兜底。
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// Service 承载 identity 的业务规则。事务边界在这里决定，不在 handler，也不在仓储。
type Service struct {
	database *db.DB
	repo     *Repository
	tokens   *auth.TokenIssuer
	// dummyHash 用于"用户不存在"时也走一遍完整的 argon2 校验。
	// 否则登录耗时本身就是一个账号探测接口：存在 → 60ms，不存在 → 1ms。
	dummyHash string
	// 邀请：链接前缀、有效期与邮件投递端口。
	// notifier 可以为 nil（测试场景），此时邀请仍会创建，只是不投递邮件。
	notifier      EmailNotifier
	inviteBaseURL string
	inviteTTL     time.Duration
}

// InvitationOptions 是邀请功能的外部依赖。BaseURL 必须与用户实际访问的
// 站点一致，否则邮件里的链接指向错误的主机。
type InvitationOptions struct {
	Notifier EmailNotifier
	BaseURL  string
	TTL      time.Duration
}

func NewService(database *db.DB, tokens *auth.TokenIssuer, invites InvitationOptions) (*Service, error) {
	dummy, err := auth.HashPassword("expensio-timing-equalizer")
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(invites.BaseURL), "/")
	if base == "" {
		return nil, errors.New("identity: 邀请链接 base URL 不能为空")
	}
	ttl := invites.TTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Service{
		database: database, repo: NewRepository(), tokens: tokens, dummyHash: dummy,
		notifier: invites.Notifier, inviteBaseURL: base, inviteTTL: ttl,
	}, nil
}

type RegisterInput struct {
	TenantSlug  string `json:"tenant_slug"`
	TenantName  string `json:"tenant_name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type LoginInput struct {
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

// Register 一次性创建租户、账号与 owner 成员关系。
// 三步必须是同一个事务：留下"有租户没人"或"有账号没身份"的半成品，
// 后续没有任何接口能修复。
func (s *Service) Register(ctx context.Context, in RegisterInput, meta RequestMeta) (*Session, error) {
	slug := strings.ToLower(strings.TrimSpace(in.TenantSlug))
	if !slugPattern.MatchString(slug) {
		return nil, apperr.InvalidArgument("租户标识只能包含小写字母、数字与连字符，且以字母或数字开头结尾")
	}

	tenantName := strings.TrimSpace(in.TenantName)
	if tenantName == "" || utf8.RuneCountInString(tenantName) > maxNameLen {
		return nil, apperr.InvalidArgument("租户名称不能为空且不超过 80 个字符")
	}

	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if err := validatePassword(in.Password); err != nil {
		return nil, err
	}

	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = email[:strings.IndexByte(email, '@')]
	}
	// 与 PATCH /me 用同一条规则：注册时也不接受控制字符或超长名称。
	displayName, err = validateDisplayName(displayName)
	if err != nil {
		return nil, err
	}

	// 哈希在事务外算：argon2 需要几十毫秒，没有必要占着一个数据库连接。
	passwordHash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, apperr.Internal(err)
	}

	tenantID := uuid.New()
	tenant := &Tenant{ID: tenantID, Slug: slug, Name: tenantName, Plan: "free", Status: "active"}
	user := &User{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
		Status:       "active",
	}

	var session *Session
	err = s.database.WithinTenantID(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := s.repo.TenantBySlug(ctx, tx, slug); err == nil {
			return apperr.Conflict("tenant.slug_taken", "该租户标识已被占用")
		} else if !IsNoRows(err) {
			return err
		}

		if _, err := s.repo.UserByEmail(ctx, tx, email); err == nil {
			return apperr.Conflict("auth.email_taken", "该邮箱已注册")
		} else if !IsNoRows(err) {
			return err
		}

		if err := s.repo.CreateTenant(ctx, tx, tenant, user.ID); err != nil {
			return err
		}
		if err := s.repo.CreateUser(ctx, tx, user); err != nil {
			return err
		}
		sub := sessionSubject{
			UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName,
			TenantID: tenant.ID, TenantSlug: tenant.Slug, TenantName: tenant.Name,
			Role: tenantctx.RoleOwner,
		}
		if err := s.repo.CreateMembership(ctx, tx, tenant.ID, user.ID, string(tenantctx.RoleOwner), "active", user.ID); err != nil {
			return err
		}
		if session, err = s.issueSession(ctx, tx, sub, uuid.Nil, meta); err != nil {
			return err
		}
		return s.audit(ctx, tx, tenant.ID, &user.ID, "auth.register", "user", user.ID.String(), meta,
			map[string]any{"tenant_slug": tenant.Slug})
	})
	if err != nil {
		switch UniqueConstraint(err) {
		case "tenants_slug_key":
			return nil, apperr.Conflict("tenant.slug_taken", "该租户标识已被占用")
		case "users_email_lower_key":
			return nil, apperr.Conflict("auth.email_taken", "该邮箱已注册")
		}
		return nil, err
	}
	return session, nil
}

// Login 分两阶段：先在无租户上下文的事务里按 slug / email 定位主体，
// 再在租户上下文里读成员关系、写刷新令牌与审计。
// 拆开的原因是密码校验不应占着事务，而成员关系必须受 RLS 约束。
func (s *Service) Login(ctx context.Context, in LoginInput, meta RequestMeta) (*Session, error) {
	slug := strings.ToLower(strings.TrimSpace(in.TenantSlug))
	if slug == "" {
		return nil, apperr.InvalidArgument("租户标识不能为空")
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if in.Password == "" {
		return nil, apperr.InvalidArgument("密码不能为空")
	}

	// 锁定检查在密码校验之前：锁定期内不做真实校验，字典攻击拿不到
	// "这个密码对不对"的信号。仍然跑一次 dummy argon2，保持与
	// "账号不存在"相同的耗时。
	if _, locked, err := s.loginLocked(ctx, email); err != nil {
		return nil, err
	} else if locked {
		_, _ = auth.VerifyPassword(s.dummyHash, in.Password)
		return nil, errTooManyAttempts()
	}

	var (
		tenant *Tenant
		user   *User
	)
	err = s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		t, err := s.repo.TenantBySlug(ctx, tx, slug)
		switch {
		case IsNoRows(err):
			return nil
		case err != nil:
			return err
		}
		tenant = t
		u, err := s.repo.UserByEmail(ctx, tx, email)
		switch {
		case IsNoRows(err):
			return nil
		case err != nil:
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 命中与不命中走同样次数的 argon2：返回文案相同还不够，耗时也必须相同。
	if tenant == nil || user == nil {
		_, _ = auth.VerifyPassword(s.dummyHash, in.Password)
		return nil, s.loginFailureResult(ctx, email)
	}
	ok, verifyErr := auth.VerifyPassword(user.PasswordHash, in.Password)
	if verifyErr != nil {
		// 库里的哈希格式非法属于数据事故，对外仍然是"凭证错误"，原因只进日志。
		// 不计数：存储故障不该把用户锁在门外。
		logger.FromContext(ctx).Error("identity: 存储的密码哈希非法", zap.String("user_id", user.ID.String()), zap.Error(verifyErr))
		return nil, errInvalidCredentials()
	}
	if !ok {
		return nil, s.loginFailureResult(ctx, email)
	}
	if user.Status != "active" {
		return nil, apperr.Forbidden("该账号已被停用")
	}
	if tenant.Status != "active" {
		return nil, apperr.Forbidden("该租户已被停用")
	}

	var session *Session
	err = s.database.WithinTenantID(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		m, err := s.repo.Membership(ctx, tx, user.ID)
		if IsNoRows(err) {
			// 密码正确但不是本租户成员时，仍然返回统一的"凭证错误"。
			// 若这里返回 403，攻击者就能用一份泄露的密码试出"账号存在且密码正确"——
			// 登录接口不应成为账号枚举的判定点。
			return errInvalidCredentials()
		}
		if err != nil {
			return err
		}
		if m.Status != "active" {
			return apperr.Forbidden("该账号在此租户中已被停用")
		}
		sub := sessionSubject{
			UserID: user.ID, Email: user.Email, DisplayName: user.DisplayName,
			TenantID: tenant.ID, TenantSlug: tenant.Slug, TenantName: tenant.Name,
			Role: tenantctx.Role(m.Role),
		}
		if session, err = s.issueSession(ctx, tx, sub, uuid.Nil, meta); err != nil {
			return err
		}
		// 成功登录清零失败计数：与登录同一个事务，提交即一致。
		if err := s.repo.ClearLoginFailures(ctx, tx, email); err != nil {
			return err
		}
		return s.audit(ctx, tx, tenant.ID, &user.ID, "auth.login", "user", user.ID.String(), meta,
			map[string]any{"tenant_slug": tenant.Slug})
	})
	if err != nil {
		return nil, err
	}
	return session, nil
}

// Refresh 轮换刷新令牌。
// 重放检测：链上已经作废的令牌再次出现，说明旧令牌被复制走了——
// 此时无法判断谁是攻击者，只能把整条链作废并让用户重新登录。
func (s *Service) Refresh(ctx context.Context, plain string, meta RequestMeta) (*Session, error) {
	if plain == "" {
		return nil, apperr.InvalidArgument("缺少刷新令牌")
	}

	rec, err := s.refreshTokenByPlain(ctx, plain)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errSessionExpired()
	}
	if rec.RevokedAt != nil {
		// ReplacedBy 非空说明这枚令牌是被轮换掉的旧令牌——它再次出现就是明确的重放信号。
		// 若只是被登出撤销，不存在"再用一次"的攻击语义，安静拒绝即可：
		// 否则每次登出后客户端的一次重试都会产生一条假的告警。
		if rec.ReplacedBy != nil {
			if err := s.revokeFamily(ctx, rec, "rotated_token_reused"); err != nil {
				return nil, err
			}
		}
		return nil, errSessionExpired()
	}
	if !time.Now().Before(rec.ExpiresAt) {
		return nil, errSessionExpired()
	}

	var (
		session  *Session
		replayed bool
	)
	err = s.database.WithinTenantID(ctx, rec.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sub, err := s.subjectByID(ctx, tx, rec.TenantID, rec.UserID)
		if err != nil {
			return err
		}

		newID := uuid.New()
		rotated, err := s.repo.RotateToken(ctx, tx, rec.ID, newID)
		if err != nil {
			return err
		}
		if !rotated {
			// 并发的第二个刷新请求：把它当作重放处理，并且必须提交撤销，
			// 所以这里是"正常返回 + 标记"，而不是直接返回错误（错误会回滚整个事务）。
			replayed = true
			return s.repo.RevokeFamily(ctx, tx, rec.FamilyID)
		}

		// RotateToken 已经写入了新令牌的主键，这里让 CreateRefreshToken 使用同一个 ID。
		if session, err = s.issueSessionWithID(ctx, tx, sub, rec.FamilyID, newID, meta); err != nil {
			return err
		}
		return s.audit(ctx, tx, rec.TenantID, &rec.UserID, "auth.refresh", "session", rec.FamilyID.String(), meta, nil)
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		return nil, errSessionExpired()
	}
	return session, nil
}

// Logout 撤销整条令牌链。令牌不存在时同样返回成功：
// 登出是幂等操作，区分"不存在"与"已撤销"只会告诉攻击者令牌是否有效。
func (s *Service) Logout(ctx context.Context, plain string, meta RequestMeta) error {
	if plain == "" {
		return apperr.InvalidArgument("缺少刷新令牌")
	}
	rec, err := s.refreshTokenByPlain(ctx, plain)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	return s.database.WithinTenantID(ctx, rec.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.RevokeFamily(ctx, tx, rec.FamilyID); err != nil {
			return err
		}
		return s.audit(ctx, tx, rec.TenantID, &rec.UserID, "auth.logout", "session", rec.FamilyID.String(), meta, nil)
	})
}

// Me 返回当前身份。角色取数据库的实时值，而不是令牌里的旧值：
// 被降级或停用的成员不该在令牌过期前继续享受旧权限。
func (s *Service) Me(ctx context.Context) (*Profile, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return nil, apperr.Unauthorized("缺少访问令牌")
	}

	var profile *Profile
	err := s.database.WithinTenantID(ctx, actor.ID, func(ctx context.Context, tx pgx.Tx) error {
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

func (s *Service) refreshTokenByPlain(ctx context.Context, plain string) (*RefreshToken, error) {
	var rec *RefreshToken
	err := s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.repo.RefreshTokenByHash(ctx, tx, auth.HashRefreshToken(plain))
		if IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		rec = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// revokeFamily 在独立事务里撤销整族令牌。
// 不能和"返回重放错误"放在同一个事务：返回错误会触发回滚，撤销就白做了。
func (s *Service) revokeFamily(ctx context.Context, rec *RefreshToken, reason string) error {
	logger.FromContext(ctx).Warn("identity: 检测到刷新令牌重放，已作废整族令牌",
		zap.String("family_id", rec.FamilyID.String()),
		zap.String("user_id", rec.UserID.String()),
		zap.String("reason", reason),
	)
	return s.database.WithinTenantID(ctx, rec.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.repo.RevokeFamily(ctx, tx, rec.FamilyID); err != nil {
			return err
		}
		return s.audit(ctx, tx, rec.TenantID, &rec.UserID, "auth.refresh_replayed", "session", rec.FamilyID.String(), RequestMeta{}, nil)
	})
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action, targetType, targetID string, meta RequestMeta, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	return s.repo.InsertAudit(ctx, tx, &AuditRecord{
		ID:          uuid.New(),
		TenantID:    tenantID,
		ActorUserID: actor,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		Meta:        meta,
		Metadata:    metadata,
	})
}
