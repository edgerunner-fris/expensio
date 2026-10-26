package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// audience 固定为本系统，防止把在别的服务签发的同密钥令牌拿来用。
const audience = "expensio-api"

// Claims 是本系统 access token 的全部载荷。
// 只放鉴权必需字段：租户、角色、会话。任何其他信息都应该现查，
// 因为令牌在有效期内是不可撤销的，塞进去就会过期。
type Claims struct {
	TenantID  string `json:"tid"`
	Role      string `json:"role"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// Tenant 把令牌载荷转成租户上下文；字段非法时返回错误而不是零值。
func (c *Claims) Tenant() (tenantctx.Tenant, error) {
	tenantID, err := uuid.Parse(c.TenantID)
	if err != nil {
		return tenantctx.Tenant{}, fmt.Errorf("auth: 令牌中的租户 ID 非法: %w", err)
	}
	userID, err := uuid.Parse(c.Subject)
	if err != nil {
		return tenantctx.Tenant{}, fmt.Errorf("auth: 令牌中的用户 ID 非法: %w", err)
	}
	role := tenantctx.Role(c.Role)
	if !role.Valid() {
		return tenantctx.Tenant{}, fmt.Errorf("auth: 令牌中的角色非法: %q", c.Role)
	}
	return tenantctx.Tenant{ID: tenantID, UserID: userID, Role: role}, nil
}

// TokenIssuer 负责签发与校验 access token。
type TokenIssuer struct {
	issuer     string
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

func NewTokenIssuer(issuer, secret string, accessTTL, refreshTTL time.Duration) (*TokenIssuer, error) {
	if issuer == "" {
		return nil, errors.New("auth: issuer 不能为空")
	}
	if len(secret) < 32 {
		return nil, errors.New("auth: 签名密钥至少需要 32 字节")
	}
	if accessTTL <= 0 || refreshTTL <= accessTTL {
		return nil, errors.New("auth: 令牌有效期非法")
	}
	return &TokenIssuer{
		issuer:     issuer,
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}, nil
}

// RefreshTTL 供服务层计算刷新令牌过期时间。
func (i *TokenIssuer) RefreshTTL() time.Duration { return i.refreshTTL }

// IssueAccess 签发 access token，同时返回过期时间。
func (i *TokenIssuer) IssueAccess(userID, tenantID uuid.UUID, role tenantctx.Role, sessionID uuid.UUID) (string, time.Time, error) {
	now := i.now()
	expiresAt := now.Add(i.accessTTL)

	claims := Claims{
		TenantID:  tenantID.String(),
		Role:      string(role),
		SessionID: sessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: 签发令牌失败: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccess 校验签名、算法、签发方、受众与有效期。
// 显式限制签名算法为 HS256：不校验算法就等于允许 alg=none 之类的降级攻击。
func (i *TokenIssuer) ParseAccess(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("auth: 非预期的签名算法 %q", token.Method.Alg())
		}
		return i.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(i.issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		// 30 秒时钟偏移容忍：网关与各服务之间可能有轻微时间差。
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: 令牌校验失败: %w", err)
	}
	return claims, nil
}
