package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// JWKS 是第三方验签方的唯一入口：公钥从这里取，不需要共享任何密钥。
func TestJWKSEndpointPublishesVerificationKey(t *testing.T) {
	h := newHarness(t)

	res, err := h.client.Get(h.server.URL + "/.well-known/jwks.json")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "public, max-age=300", res.Header.Get("Cache-Control"))

	var body auth.JWKS
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Len(t, body.Keys, 1)
	key := body.Keys[0]
	assert.Equal(t, "OKP", key.Kty)
	assert.Equal(t, "Ed25519", key.Crv)
	assert.Equal(t, "EdDSA", key.Alg)
	assert.Equal(t, "sig", key.Use)
	assert.NotEmpty(t, key.Kid)
	assert.NotEmpty(t, key.X)

	// 用 JWKS 里的公钥构造一个"只能验签"的 issuer，验证它确实能校验
	// identity 签发的令牌——而不是仅仅把字段原样回显。
	raw, err := base64.RawURLEncoding.DecodeString(key.X)
	require.NoError(t, err)
	require.Len(t, raw, ed25519.PublicKeySize)

	verifier, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer:     testIssuer,
		PublicKeys: []ed25519.PublicKey{ed25519.PublicKey(raw)},
		AccessTTL:  testAccessTTL,
		RefreshTTL: testRefreshTTL,
	})
	require.NoError(t, err)

	token, _, err := h.tokens.IssueAccess(uuid.New(), uuid.New(), tenantctx.RoleMember, uuid.New())
	require.NoError(t, err)
	claims, err := verifier.ParseAccess(token)
	require.NoError(t, err)
	assert.Equal(t, string(tenantctx.RoleMember), claims.Role)
}
