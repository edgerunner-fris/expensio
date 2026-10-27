package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func invitationToken(t *testing.T, created CreatedInvitation) string {
	t.Helper()
	const prefix = "http://invite.test/invite/"
	require.True(t, strings.HasPrefix(created.Link, prefix), "链接前缀异常: %s", created.Link)
	token := strings.TrimPrefix(created.Link, prefix)
	require.NotEmpty(t, token)
	return token
}

func (h *harness) createInvitation(token, email, role string) CreatedInvitation {
	h.t.Helper()
	resp := h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": email, "role": role}, token)
	require.Equal(h.t, http.StatusCreated, resp.Status, resp.Message)
	var created CreatedInvitation
	require.NoError(h.t, json.Unmarshal(resp.Data, &created))
	return created
}

func TestInvitationLifecycleForNewUser(t *testing.T) {
	h := newHarness(t)
	owner := h.register()

	// 同一个包内的测试共享一个 Postgres；-count=5 时账号表不会重置，
	// 固定邮箱会在第二轮变成"已有账号"。每次都换一个全新邮箱。
	invitee := "newbie-" + uuid.NewString()[:8] + "@example.com"

	created := h.createInvitation(owner.AccessToken, invitee, "member")
	require.NotEmpty(t, created.Link)
	assert.True(t, created.EmailQueued, "配置了 notifier 时应记录为已入队")
	assert.NotEqual(t, uuid.Nil, created.ID)

	email, ok := h.notifier.last()
	require.True(t, ok)
	assert.Equal(t, created.Link, email.Link)
	assert.Equal(t, invitee, email.Recipient)
	assert.Equal(t, owner.Profile.TenantName, email.TenantName)

	token := invitationToken(t, created)

	// 公开接口不需要登录。
	resp := h.call(http.MethodGet, "/api/v1/invitations/"+token, nil, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var info InvitationInfo
	require.NoError(t, json.Unmarshal(resp.Data, &info))
	assert.False(t, info.AccountExists)
	assert.Equal(t, invitee, info.Email)
	assert.Equal(t, "member", info.Role)
	assert.Equal(t, owner.Profile.TenantName, info.TenantName)

	resp = h.call(http.MethodPost, "/api/v1/invitations/"+token+"/accept",
		map[string]string{"password": testPassword, "display_name": "Newbie"}, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var session Session
	require.NoError(t, json.Unmarshal(resp.Data, &session))
	assert.Equal(t, invitee, session.Profile.Email)
	assert.Equal(t, "Newbie", session.Profile.DisplayName)
	assert.Equal(t, "member", session.Profile.Role)
	assert.Equal(t, owner.Profile.TenantID, session.Profile.TenantID)
	assert.NotEmpty(t, session.AccessToken)

	// 账号确实可用：用设置好的密码登录同一租户。
	login := h.login(owner.Profile.TenantSlug, invitee)
	assert.Equal(t, session.Profile.UserID, login.Profile.UserID)

	// 链接是一次性的。
	resp = h.call(http.MethodPost, "/api/v1/invitations/"+token+"/accept",
		map[string]string{"password": testPassword}, "")
	assert.Equal(t, http.StatusGone, resp.Status)
	assert.Equal(t, "invitation.accepted", resp.Code)

	// 成员列表能看到新成员。
	resp = h.call(http.MethodGet, "/api/v1/tenants/current/members?page_size=100", nil, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var page struct {
		Items []Member `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &page))
	emails := make([]string, 0, len(page.Items))
	for _, m := range page.Items {
		emails = append(emails, m.Email)
	}
	assert.Contains(t, emails, invitee)

	// 审计记录里能追到邀请与接受，且不含链接或令牌。
	resp = h.call(http.MethodGet, "/api/v1/audit-logs?page_size=100", nil, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var audit struct {
		Items []AuditEntry `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &audit))
	actions := make([]string, 0, len(audit.Items))
	for _, e := range audit.Items {
		actions = append(actions, e.Action)
		assert.NotContains(t, e.Metadata, "token")
		assert.NotContains(t, e.Metadata, "link")
	}
	joined := strings.Join(actions, ",")
	assert.Contains(t, joined, "member.invited")
	assert.Contains(t, joined, "member.invitation_accepted")
}

func TestInvitationPermissions(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	tenantID := owner.Profile.TenantID
	admin := h.seedMember(tenantID, tenantctx.RoleAdmin)
	member := h.seedMember(tenantID, tenantctx.RoleMember)
	viewer := h.seedMember(tenantID, tenantctx.RoleViewer)

	// admin 可以邀请 member，但不能邀请 owner。
	h.createInvitation(admin.AccessToken, "by-admin@example.com", "member")
	resp := h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": "owner-by-admin@example.com", "role": "owner"}, admin.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status)
	assert.Equal(t, "auth.forbidden", resp.Code)

	// owner 可以邀请 owner。
	h.createInvitation(owner.AccessToken, "second-owner@example.com", "owner")

	// member / viewer 不能创建或读取邀请。
	for _, s := range []Session{member, viewer} {
		resp = h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
			map[string]string{"email": "nope@example.com", "role": "member"}, s.AccessToken)
		assert.Equal(t, http.StatusForbidden, resp.Status)
		resp = h.call(http.MethodGet, "/api/v1/tenants/current/invitations", nil, s.AccessToken)
		assert.Equal(t, http.StatusForbidden, resp.Status)
	}

	// 未登录不能创建。
	resp = h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": "anon@example.com", "role": "member"}, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status)
}

func TestInvitationReinviteAndRevoke(t *testing.T) {
	h := newHarness(t)
	owner := h.register()

	first := h.createInvitation(owner.AccessToken, "repeat@example.com", "member")
	firstToken := invitationToken(t, first)

	second := h.createInvitation(owner.AccessToken, "repeat@example.com", "viewer")
	secondToken := invitationToken(t, second)
	require.NotEqual(t, firstToken, secondToken)

	// 旧链接立即失效。
	resp := h.call(http.MethodGet, "/api/v1/invitations/"+firstToken, nil, "")
	assert.Equal(t, http.StatusGone, resp.Status)
	assert.Equal(t, "invitation.revoked", resp.Code)

	// 新链接仍然可用，角色是第二次邀请的 viewer。
	resp = h.call(http.MethodGet, "/api/v1/invitations/"+secondToken, nil, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var info InvitationInfo
	require.NoError(t, json.Unmarshal(resp.Data, &info))
	assert.Equal(t, "viewer", info.Role)

	// 撤销后同样失效。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current/invitations/"+second.ID.String(), nil, owner.AccessToken)
	require.Equal(t, http.StatusNoContent, resp.Status, resp.Message)

	resp = h.call(http.MethodGet, "/api/v1/invitations/"+secondToken, nil, "")
	assert.Equal(t, http.StatusGone, resp.Status)

	// 重复撤销返回 409 而不是 404：邀请存在，只是已不在等待状态。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current/invitations/"+second.ID.String(), nil, owner.AccessToken)
	assert.Equal(t, http.StatusConflict, resp.Status)
	assert.Equal(t, "invitation.not_pending", resp.Code)

	// 不存在的邀请 ID 是 404。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current/invitations/"+uuid.NewString(), nil, owner.AccessToken)
	assert.Equal(t, http.StatusNotFound, resp.Status)
}

func TestInvitationExpires(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	created := h.createInvitation(owner.AccessToken, "late@example.com", "member")
	token := invitationToken(t, created)

	require.NoError(t, h.db.WithinTenantID(context.Background(), owner.Profile.TenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE app.invitations SET expires_at = now() - interval '1 minute' WHERE id = $1`,
				created.ID)
			return err
		}))

	resp := h.call(http.MethodGet, "/api/v1/invitations/"+token, nil, "")
	assert.Equal(t, http.StatusGone, resp.Status)
	assert.Equal(t, "invitation.expired", resp.Code)

	resp = h.call(http.MethodPost, "/api/v1/invitations/"+token+"/accept",
		map[string]string{"password": testPassword}, "")
	assert.Equal(t, http.StatusGone, resp.Status)
}

func TestInvitationForExistingAccountRequiresPassword(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	// 另一个租户的 owner 是全局账号，但还不是本租户成员。
	other := h.register()

	created := h.createInvitation(owner.AccessToken, other.Profile.Email, "admin")
	token := invitationToken(t, created)

	resp := h.call(http.MethodGet, "/api/v1/invitations/"+token, nil, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var info InvitationInfo
	require.NoError(t, json.Unmarshal(resp.Data, &info))
	assert.True(t, info.AccountExists, "邮箱已有全局账号时必须走密码确认路径")

	resp = h.call(http.MethodPost, "/api/v1/invitations/"+token+"/accept",
		map[string]string{"password": "wrong-password-123"}, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status)
	assert.Equal(t, "auth.unauthorized", resp.Code)

	resp = h.call(http.MethodPost, "/api/v1/invitations/"+token+"/accept",
		map[string]string{"password": testPassword}, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var session Session
	require.NoError(t, json.Unmarshal(resp.Data, &session))
	assert.Equal(t, other.Profile.UserID, session.Profile.UserID, "已有账号应复用原用户，而不是创建新用户")
	assert.Equal(t, owner.Profile.TenantID, session.Profile.TenantID)
	assert.Equal(t, "admin", session.Profile.Role)

	// 原租户的会话不受影响。
	assert.Equal(t, other.Profile.TenantID, h.login(other.Profile.TenantSlug, other.Profile.Email).Profile.TenantID)
}

func TestInvitationTargetRules(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	member := h.seedMember(owner.Profile.TenantID, tenantctx.RoleMember)

	resp := h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": member.Profile.Email, "role": "viewer"}, owner.AccessToken)
	assert.Equal(t, http.StatusConflict, resp.Status)
	assert.Equal(t, "member.already_member", resp.Code)

	// 非法角色与非法邮箱在创建前就被拒绝。
	resp = h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": "x@example.com", "role": "superuser"}, owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status)

	resp = h.call(http.MethodPost, "/api/v1/tenants/current/invitations",
		map[string]string{"email": "not-an-email", "role": "member"}, owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status)
}

func TestInvitationUnknownToken(t *testing.T) {
	h := newHarness(t)

	for _, token := range []string{"not-a-token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		resp := h.call(http.MethodGet, "/api/v1/invitations/"+token, nil, "")
		assert.Equal(t, http.StatusNotFound, resp.Status, "token=%s", token)
	}
}
