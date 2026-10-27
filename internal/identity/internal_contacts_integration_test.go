package identity

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestInternalTenantContactsRequiresSignedTenantToken(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	member := h.seedMember(owner.Profile.TenantID, tenantctx.RoleMember)

	// 无令牌与普通用户 access token 都必须被拒绝。
	assert.Equal(t, http.StatusUnauthorized,
		h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, "").Status)
	assert.Equal(t, http.StatusUnauthorized,
		h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, owner.AccessToken).Status,
		"用户 JWT 不能当作服务间令牌")

	// 篡改签名必须失败。
	valid, err := serviceauth.Sign(testInternalToken, owner.Profile.TenantID, time.Minute, time.Now())
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, valid+"x").Status)

	resp := h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, valid)
	require.Equal(t, http.StatusOK, resp.Status)
	require.Equal(t, "ok", resp.Code)

	var contacts []Member
	require.NoError(t, json.Unmarshal(resp.Data, &contacts))
	emails := make([]string, 0, len(contacts))
	for _, c := range contacts {
		emails = append(emails, c.Email)
	}
	assert.Contains(t, emails, owner.Profile.Email)
	assert.Contains(t, emails, member.Profile.Email)
}

func TestInternalTenantContactsCannotReadAnotherTenant(t *testing.T) {
	h := newHarness(t)
	owner := h.register()

	otherTenant := uuid.New()
	token, err := serviceauth.Sign(testInternalToken, otherTenant, time.Minute, time.Now())
	require.NoError(t, err)

	resp := h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, token)
	require.Equal(t, http.StatusOK, resp.Status)

	var contacts []Member
	require.NoError(t, json.Unmarshal(resp.Data, &contacts))
	assert.Empty(t, contacts, "签名令牌声明的租户没有成员时只能得到空列表")

	// 同一个接口用原租户的令牌仍能正常返回，说明隔离来自签名载荷而不是全局拒绝。
	ownerToken, err := serviceauth.Sign(testInternalToken, owner.Profile.TenantID, time.Minute, time.Now())
	require.NoError(t, err)
	resp = h.call(http.MethodGet, "/api/v1/internal/tenant-contacts", nil, ownerToken)
	require.Equal(t, http.StatusOK, resp.Status)
	require.NoError(t, json.Unmarshal(resp.Data, &contacts))
	require.Len(t, contacts, 1)
	assert.Equal(t, owner.Profile.Email, contacts[0].Email)
}
