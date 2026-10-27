package auth

import (
	"crypto/ed25519"
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

// IssuerConfig 描述一个签发/校验器。
//
// PrivateKey 只有 identity 持有：持有它才能签发任意租户的令牌。
// 其他服务只配置 PublicKeys，因此即使被攻破也无法伪造令牌——
// 这正是从 HS256 迁移到 EdDSA 的目的。
// HS256Secret 用于迁移期继续接受旧令牌；迁移完成后置空即可拒绝该算法。
type IssuerConfig struct {
	Issuer      string
	HS256Secret string
	PrivateKey  ed25519.PrivateKey
	PublicKeys  []ed25519.PublicKey
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
}

// TokenIssuer 负责签发与校验 access token。
type TokenIssuer struct {
	issuer     string
	secret     []byte
	signKey    ed25519.PrivateKey
	verifyKeys map[string]ed25519.PublicKey
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

func NewTokenIssuer(cfg IssuerConfig) (*TokenIssuer, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("auth: issuer 不能为空")
	}
	if cfg.AccessTTL <= 0 || cfg.RefreshTTL <= cfg.AccessTTL {
		return nil, errors.New("auth: 令牌有效期非法")
	}

	hasEd25519 := cfg.PrivateKey != nil || len(cfg.PublicKeys) > 0
	if !hasEd25519 && len(cfg.HS256Secret) < 32 {
		return nil, errors.New("auth: 需要 Ed25519 公钥，或至少 32 字节的 HS256 密钥")
	}
	if cfg.HS256Secret != "" && len(cfg.HS256Secret) < 32 {
		return nil, errors.New("auth: HS256 密钥至少需要 32 字节")
	}
	if cfg.PrivateKey != nil {
		switch len(cfg.PrivateKey) {
		case ed25519.SeedSize:
			cfg.PrivateKey = ed25519.NewKeyFromSeed(cfg.PrivateKey)
		case ed25519.PrivateKeySize:
		default:
			return nil, fmt.Errorf("auth: Ed25519 私钥长度非法: %d", len(cfg.PrivateKey))
		}
	}

	keys := make(map[string]ed25519.PublicKey, len(cfg.PublicKeys)+1)
	for _, pub := range cfg.PublicKeys {
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("auth: Ed25519 公钥长度非法: %d", len(pub))
		}
		keys[KeyID(pub)] = pub
	}
	// 签发者必须能校验自己刚签出的令牌，否则配置漏掉当前公钥时会立刻 401。
	if cfg.PrivateKey != nil {
		pub := cfg.PrivateKey.Public().(ed25519.PublicKey)
		keys[KeyID(pub)] = pub
	}

	return &TokenIssuer{
		issuer:     cfg.Issuer,
		secret:     []byte(cfg.HS256Secret),
		signKey:    cfg.PrivateKey,
		verifyKeys: keys,
		accessTTL:  cfg.AccessTTL,
		refreshTTL: cfg.RefreshTTL,
		now:        time.Now,
	}, nil
}

// RefreshTTL 供服务层计算刷新令牌过期时间。
func (i *TokenIssuer) RefreshTTL() time.Duration { return i.refreshTTL }

// IssueAccess 签发 access token，同时返回过期时间。
// 有私钥时用 EdDSA（header 带 kid），否则退化为迁移期的 HS256。
func (i *TokenIssuer) IssueAccess(userID, tenantID uuid.UUID, role tenantctx.Role, sessionID uuid.UUID) (string, time.Time, error) {
	now := i.now()
	expiresAt := now.Add(i.accessTTL)

	claims := i.claims(userID, tenantID, role, sessionID, now, expiresAt)

	var (
		method jwt.SigningMethod
		key    any
	)
	switch {
	case i.signKey != nil:
		method = jwt.SigningMethodEdDSA
		key = i.signKey
	case len(i.secret) >= 32:
		method = jwt.SigningMethodHS256
		key = i.secret
	default:
		return "", time.Time{}, errors.New("auth: 当前配置没有签名密钥")
	}

	token := jwt.NewWithClaims(method, claims)
	if i.signKey != nil {
		token.Header["kid"] = KeyID(i.signKey.Public().(ed25519.PublicKey))
	}

	signed, err := token.SignedString(key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: 签发令牌失败: %w", err)
	}
	return signed, expiresAt, nil
}

func (i *TokenIssuer) claims(userID, tenantID uuid.UUID, role tenantctx.Role, sessionID uuid.UUID, now, expiresAt time.Time) Claims {
	return Claims{
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
}

// ParseAccess 校验签名、算法、签发方、受众与有效期。
// 显式限制允许的算法：不校验算法就等于允许 alg=none 之类的降级攻击。
// EdDSA 令牌按 header 的 kid 选公钥，未知 kid 直接拒绝——
// 不做"试所有公钥"，避免用错误密钥耗尽 CPU。
func (i *TokenIssuer) ParseAccess(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		switch token.Method.Alg() {
		case jwt.SigningMethodEdDSA.Alg():
			kid, _ := token.Header["kid"].(string)
			pub, ok := i.verifyKeys[kid]
			if !ok {
				return nil, fmt.Errorf("auth: 未知的签名密钥 %q", kid)
			}
			return pub, nil
		case jwt.SigningMethodHS256.Alg():
			if len(i.secret) < 32 {
				return nil, errors.New("auth: 已禁用 HS256 令牌")
			}
			return i.secret, nil
		default:
			return nil, fmt.Errorf("auth: 非预期的签名算法 %q", token.Method.Alg())
		}
	},
		jwt.WithValidMethods(i.validMethods()),
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

func (i *TokenIssuer) validMethods() []string {
	methods := make([]string, 0, 2)
	if len(i.verifyKeys) > 0 {
		methods = append(methods, jwt.SigningMethodEdDSA.Alg())
	}
	if len(i.secret) >= 32 {
		methods = append(methods, jwt.SigningMethodHS256.Alg())
	}
	return methods
}
