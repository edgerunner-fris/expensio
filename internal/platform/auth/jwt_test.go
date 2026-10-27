package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const testHS256Secret = "test-hs256-secret-at-least-32-bytes!"

func newSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv
}

func newIssuer(t *testing.T, cfg IssuerConfig) *TokenIssuer {
	t.Helper()
	if cfg.Issuer == "" {
		cfg.Issuer = "expensio-test"
	}
	if cfg.AccessTTL == 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL == 0 {
		cfg.RefreshTTL = 24 * time.Hour
	}
	issuer, err := NewTokenIssuer(cfg)
	require.NoError(t, err)
	return issuer
}

func issueClaimSet(t *testing.T, issuer *TokenIssuer) (string, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	userID, tenantID, sessionID := uuid.New(), uuid.New(), uuid.New()
	raw, _, err := issuer.IssueAccess(userID, tenantID, tenantctx.RoleAdmin, sessionID)
	require.NoError(t, err)
	return raw, userID, tenantID, sessionID
}

func TestEd25519IssueAndParse(t *testing.T) {
	priv := newSigningKey(t)
	issuer := newIssuer(t, IssuerConfig{PrivateKey: priv})

	raw, userID, tenantID, sessionID := issueClaimSet(t, issuer)

	header := decodeHeader(t, raw)
	assert.Equal(t, "EdDSA", header["alg"])
	assert.Equal(t, KeyID(priv.Public().(ed25519.PublicKey)), header["kid"])

	claims, err := issuer.ParseAccess(raw)
	require.NoError(t, err)
	assert.Equal(t, tenantID.String(), claims.TenantID)
	assert.Equal(t, userID.String(), claims.Subject)
	assert.Equal(t, sessionID.String(), claims.SessionID)
	assert.Equal(t, string(tenantctx.RoleAdmin), claims.Role)

	tenant, err := claims.Tenant()
	require.NoError(t, err)
	assert.Equal(t, tenantID, tenant.ID)
	assert.Equal(t, userID, tenant.UserID)
	assert.Equal(t, tenantctx.RoleAdmin, tenant.Role)
}

// 只有公钥的服务可以验签，但无法签发：私钥不离开 identity。
func TestVerifyOnlyIssuerCannotSign(t *testing.T) {
	priv := newSigningKey(t)
	signer := newIssuer(t, IssuerConfig{PrivateKey: priv})
	verifier := newIssuer(t, IssuerConfig{PublicKeys: []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}})

	raw, _, _, _ := issueClaimSet(t, signer)
	_, err := verifier.ParseAccess(raw)
	require.NoError(t, err)

	_, _, err = verifier.IssueAccess(uuid.New(), uuid.New(), tenantctx.RoleViewer, uuid.New())
	assert.Error(t, err)
}

// 用另一把私钥签出的令牌，kid 不在验签方的公钥集合里，必须拒绝。
func TestUnknownKidIsRejected(t *testing.T) {
	signerKey := newSigningKey(t)
	otherKey := newSigningKey(t)
	signer := newIssuer(t, IssuerConfig{PrivateKey: signerKey})
	verifier := newIssuer(t, IssuerConfig{PublicKeys: []ed25519.PublicKey{otherKey.Public().(ed25519.PublicKey)}})

	raw, _, _, _ := issueClaimSet(t, signer)
	_, err := verifier.ParseAccess(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "校验失败")
}

// 未配置 HS256 密钥时，即便攻击者知道某个字符串也不能用 HS256 伪造。
func TestHS256RejectedWhenDisabled(t *testing.T) {
	priv := newSigningKey(t)
	issuer := newIssuer(t, IssuerConfig{PrivateKey: priv})

	forged := signHS256(t, "attacker-controlled-secret-at-least-32-bytes!", time.Now().Add(time.Hour))
	_, err := issuer.ParseAccess(forged)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "校验失败")
}

// 迁移期：新的 EdDSA 令牌与旧的 HS256 令牌同时可用；关掉开关后 HS256 立即失效。
func TestHS256AcceptedOnlyDuringMigration(t *testing.T) {
	priv := newSigningKey(t)

	migrating := newIssuer(t, IssuerConfig{PrivateKey: priv, HS256Secret: testHS256Secret})
	legacy := signHS256(t, testHS256Secret, time.Now().Add(time.Hour))
	_, err := migrating.ParseAccess(legacy)
	require.NoError(t, err)

	// 同一个签发器仍应优先签发 EdDSA。
	newToken, _, _, _ := issueClaimSet(t, migrating)
	assert.Equal(t, "EdDSA", decodeHeader(t, newToken)["alg"])

	afterMigration := newIssuer(t, IssuerConfig{PrivateKey: priv})
	_, err = afterMigration.ParseAccess(legacy)
	assert.Error(t, err)
}

// 篡改载荷会让签名失效，即使 kid 正确。
func TestTamperedTokenRejected(t *testing.T) {
	priv := newSigningKey(t)
	issuer := newIssuer(t, IssuerConfig{PrivateKey: priv})
	raw, _, _, _ := issueClaimSet(t, issuer)

	parts := strings.Split(raw, ".")
	require.Len(t, parts, 3)
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	_, err := issuer.ParseAccess(tampered)
	assert.Error(t, err)
}

func TestExpiredTokenRejected(t *testing.T) {
	priv := newSigningKey(t)
	issuer := newIssuer(t, IssuerConfig{PrivateKey: priv})
	issuer.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }

	raw, _, _, _ := issueClaimSet(t, issuer)
	_, err := issuer.ParseAccess(raw)
	require.Error(t, err)
}

func TestJWKSExposesVerifyKeys(t *testing.T) {
	priv := newSigningKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	issuer := newIssuer(t, IssuerConfig{PrivateKey: priv})

	jwks := issuer.JWKS()
	require.Len(t, jwks.Keys, 1)
	key := jwks.Keys[0]
	assert.Equal(t, "OKP", key.Kty)
	assert.Equal(t, "Ed25519", key.Crv)
	assert.Equal(t, "EdDSA", key.Alg)
	assert.Equal(t, "sig", key.Use)
	assert.Equal(t, KeyID(pub), key.Kid)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(pub), key.X)
}

func TestJWKSIsSortedByKid(t *testing.T) {
	keys := []ed25519.PublicKey{
		newSigningKey(t).Public().(ed25519.PublicKey),
		newSigningKey(t).Public().(ed25519.PublicKey),
		newSigningKey(t).Public().(ed25519.PublicKey),
	}
	issuer := newIssuer(t, IssuerConfig{PublicKeys: keys})
	jwks := issuer.JWKS()
	require.Len(t, jwks.Keys, 3)
	for i := 1; i < len(jwks.Keys); i++ {
		assert.Less(t, jwks.Keys[i-1].Kid, jwks.Keys[i].Kid)
	}
}

func TestKeyIDIsStable(t *testing.T) {
	pub := newSigningKey(t).Public().(ed25519.PublicKey)
	// RFC 7638 指纹只取决于公钥本身，同一公钥在任何进程都得到同一个 kid。
	assert.Equal(t, KeyID(pub), KeyID(pub))
	assert.NotEqual(t, KeyID(pub), KeyID(newSigningKey(t).Public().(ed25519.PublicKey)))
}

func TestParsePrivateKeyAcceptsSeedAndFullKey(t *testing.T) {
	priv := newSigningKey(t)

	fromFull, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv))
	require.NoError(t, err)
	assert.True(t, priv.Equal(fromFull))

	fromSeed, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv.Seed()))
	require.NoError(t, err)
	assert.True(t, priv.Equal(fromSeed))

	_, err = ParsePrivateKey("not-base64!!!")
	assert.Error(t, err)
	_, err = ParsePrivateKey(base64.StdEncoding.EncodeToString(make([]byte, 10)))
	assert.Error(t, err)
}

func TestParsePublicKeysAcceptsStandardAndURLBase64(t *testing.T) {
	pub := newSigningKey(t).Public().(ed25519.PublicKey)

	for name, encoded := range map[string]string{
		"标准填充":   base64.StdEncoding.EncodeToString(pub),
		"标准无填充":  base64.RawStdEncoding.EncodeToString(pub),
		"URL 安全": base64.RawURLEncoding.EncodeToString(pub),
	} {
		t.Run(name, func(t *testing.T) {
			keys, err := ParsePublicKeys(" " + encoded + " , ")
			require.NoError(t, err)
			require.Len(t, keys, 1)
			assert.Equal(t, pub, keys[0])
		})
	}

	_, err := ParsePublicKeys(base64.StdEncoding.EncodeToString(make([]byte, 10)))
	assert.Error(t, err)
}

func TestConfigRejectsMissingSigningMaterial(t *testing.T) {
	_, err := NewTokenIssuer(IssuerConfig{Issuer: "expensio", HS256Secret: "short"})
	assert.Error(t, err)

	_, err = NewTokenIssuer(IssuerConfig{Issuer: ""})
	assert.Error(t, err)

	_, err = NewTokenIssuer(IssuerConfig{
		Issuer: "expensio", AccessTTL: time.Hour, RefreshTTL: time.Minute,
	})
	assert.Error(t, err)
}

// ---- helpers ----

func decodeHeader(t *testing.T, raw string) map[string]any {
	t.Helper()
	parts := strings.Split(raw, ".")
	require.Len(t, parts, 3)
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var header map[string]any
	require.NoError(t, json.Unmarshal(rawHeader, &header))
	return header
}

func signHS256(t *testing.T, secret string, expiresAt time.Time) string {
	t.Helper()
	claims := Claims{
		TenantID: uuid.NewString(),
		Role:     string(tenantctx.RoleOwner),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "expensio-test",
			Subject:   uuid.NewString(),
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return raw
}
