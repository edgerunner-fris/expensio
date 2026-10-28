package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTenant 直接写库建一个空租户（无成员）。测试用它构造"同一账号属于
// 多个租户"的场景，不依赖任何尚未实现的建租户 API。
func (h *harness) createTenant(name string) uuid.UUID {
	h.t.Helper()
	tenantID := uuid.New()
	require.NoError(h.t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.tenants (id, slug, name, plan, status)
		                        VALUES ($1, $2, $3, 'free', 'active')`,
			tenantID, uniqueSlug(name), "Tenant "+name)
		return err
	}))
	return tenantID
}

func (h *harness) addMembership(tenantID, userID uuid.UUID, role string) {
	h.t.Helper()
	require.NoError(h.t, h.db.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return NewRepository().CreateMembership(ctx, tx, tenantID, userID, role, "active", userID)
		}))
}

func (h *harness) auditCount(tenantID uuid.UUID, action string) int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.db.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.audit_logs WHERE action = $1`, action).Scan(&n)
		}))
	return n
}

func TestTenantSwitch(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	tenantA := owner.Profile.TenantID

	tenantB := h.createTenant("beta")
	h.addMembership(tenantB, owner.Profile.UserID, "admin")

	// 列表：两条，A 是当前租户，B 带 B 里的角色。
	resp := h.call(http.MethodGet, "/api/v1/auth/tenants", nil, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var list struct {
		Items []TenantMembership `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &list))
	require.Len(t, list.Items, 2)

	byTenant := map[uuid.UUID]TenantMembership{}
	for _, item := range list.Items {
		byTenant[item.TenantID] = item
	}
	assert.True(t, byTenant[tenantA].Current)
	assert.Equal(t, "owner", byTenant[tenantA].Role)
	assert.False(t, byTenant[tenantB].Current)
	assert.Equal(t, "admin", byTenant[tenantB].Role)
	assert.Equal(t, "active", byTenant[tenantB].Status)

	// 切换到 B：新会话的 profile 必须反映 B 的角色。
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantB.String()}, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var switched Session
	require.NoError(t, json.Unmarshal(resp.Data, &switched))
	assert.Equal(t, tenantB, switched.Profile.TenantID)
	assert.Equal(t, "admin", switched.Profile.Role)
	assert.NotEqual(t, owner.AccessToken, switched.AccessToken)

	me := h.call(http.MethodGet, "/api/v1/me", nil, switched.AccessToken)
	require.Equal(t, http.StatusOK, me.Status, me.Message)
	var profile Profile
	require.NoError(t, json.Unmarshal(me.Data, &profile))
	assert.Equal(t, tenantB, profile.TenantID)
	assert.Equal(t, "admin", profile.Role)

	assert.Equal(t, 1, h.auditCount(tenantB, "auth.tenant_switched"))

	// 切换不是撤销凭据：原租户的刷新链仍然可用（多标签页各自停留）。
	refresh := h.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]any{"refresh_token": owner.RefreshToken}, "")
	require.Equal(t, http.StatusOK, refresh.Status, refresh.Message)

	// 切到当前租户：400，不做无意义的刷新。
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantB.String()}, switched.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status, resp.Message)

	// 不是成员：403，且不区分"租户不存在"。
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": uuid.NewString()}, switched.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status, resp.Message)

	// 被暂停的租户：403。
	tenantC := h.createTenant("gamma")
	h.addMembership(tenantC, owner.Profile.UserID, "member")
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.tenants SET status = 'suspended' WHERE id = $1`, tenantC)
		return err
	}))
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantC.String()}, switched.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status, resp.Message)

	// 停用的成员关系：403。
	tenantD := h.createTenant("delta")
	h.addMembership(tenantD, owner.Profile.UserID, "member")
	require.NoError(t, h.db.WithinTenantID(context.Background(), tenantD,
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE app.memberships SET status = 'disabled' WHERE tenant_id = $1 AND user_id = $2`,
				tenantD, owner.Profile.UserID)
			return err
		}))
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantD.String()}, switched.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status, resp.Message)
}

func TestTenantListRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.call(http.MethodGet, "/api/v1/auth/tenants", nil, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status, resp.Message)
}

func TestUpdateProfile(t *testing.T) {
	h := newHarness(t)
	owner := h.register()

	// 普通成员也能改自己的展示名。
	member := h.seedMember(owner.Profile.TenantID, "member")

	resp := h.call(http.MethodPatch, "/api/v1/me",
		map[string]any{"display_name": "  New Name  "}, member.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var updated Profile
	require.NoError(t, json.Unmarshal(resp.Data, &updated))
	assert.Equal(t, "New Name", updated.DisplayName)

	me := h.call(http.MethodGet, "/api/v1/me", nil, member.AccessToken)
	require.Equal(t, http.StatusOK, me.Status, me.Message)
	var current Profile
	require.NoError(t, json.Unmarshal(me.Data, &current))
	assert.Equal(t, "New Name", current.DisplayName)

	assert.Equal(t, 1, h.auditCount(owner.Profile.TenantID, "user.profile_updated"))

	// 审计读模型带上来源 IP 与 UA：审计页要回答"谁从哪里做的"。
	resp = h.call(http.MethodGet, "/api/v1/audit-logs?page_size=100", nil, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var logs struct {
		Items []AuditEntry `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &logs))
	var updatedEntry *AuditEntry
	for i := range logs.Items {
		if logs.Items[i].Action == "user.profile_updated" {
			updatedEntry = &logs.Items[i]
			break
		}
	}
	require.NotNil(t, updatedEntry)
	assert.Equal(t, "127.0.0.1", updatedEntry.IP)
	assert.NotEmpty(t, updatedEntry.UserAgent)

	for name, body := range map[string]any{
		"空名称":  map[string]any{"display_name": "   "},
		"超长":   map[string]any{"display_name": strings.Repeat("名", 65)},
		"控制字符": map[string]any{"display_name": "a\nb"},
		"缺字段":  map[string]any{},
	} {
		resp := h.call(http.MethodPatch, "/api/v1/me", body, member.AccessToken)
		assert.Equal(t, http.StatusBadRequest, resp.Status, name+": "+resp.Message)
	}
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t)
	session := h.register()
	const newPassword = "Expensio-Rotated-2026"

	resp := h.call(http.MethodPost, "/api/v1/auth/change-password", map[string]any{
		"current_password": "not-the-password",
		"new_password":     newPassword,
	}, session.AccessToken)
	assert.Equal(t, http.StatusUnauthorized, resp.Status, resp.Message)

	// 新密码与当前密码相同：400。
	resp = h.call(http.MethodPost, "/api/v1/auth/change-password", map[string]any{
		"current_password": testPassword,
		"new_password":     testPassword,
	}, session.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status, resp.Message)

	// 新密码太短：400。
	resp = h.call(http.MethodPost, "/api/v1/auth/change-password", map[string]any{
		"current_password": testPassword,
		"new_password":     "short",
	}, session.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status, resp.Message)

	resp = h.call(http.MethodPost, "/api/v1/auth/change-password", map[string]any{
		"current_password": testPassword,
		"new_password":     newPassword,
	}, session.AccessToken)
	require.Equal(t, http.StatusNoContent, resp.Status, resp.Message)

	// 改密撤销全部刷新链。
	resp = h.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]any{"refresh_token": session.RefreshToken}, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status, resp.Message)

	// 旧密码不再可用；新密码可登录。
	resp = h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": session.Profile.TenantSlug,
		"email":       session.Profile.Email,
		"password":    testPassword,
	}, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status, resp.Message)

	resp = h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": session.Profile.TenantSlug,
		"email":       session.Profile.Email,
		"password":    newPassword,
	}, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)

	assert.Equal(t, 1, h.auditCount(session.Profile.TenantID, "auth.password_changed"))
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t)
	session := h.register()
	slug, email := session.Profile.TenantSlug, session.Profile.Email

	wrong := func() apiResponse {
		return h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
			"tenant_slug": slug, "email": email, "password": "wrong-password-123",
		}, "")
	}

	for i := 0; i < maxLoginFailures-1; i++ {
		resp := wrong()
		require.Equal(t, http.StatusUnauthorized, resp.Status, fmt.Sprintf("第 %d 次失败", i+1))
	}
	// 达到阈值的那一次直接 429。
	resp := wrong()
	require.Equal(t, http.StatusTooManyRequests, resp.Status, resp.Message)
	assert.Equal(t, "auth.too_many_attempts", resp.Code)

	// 锁定期内即使密码正确也拒绝：不给字典攻击留下"密码对了吗"的信号。
	resp = h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": slug, "email": email, "password": testPassword,
	}, "")
	require.Equal(t, http.StatusTooManyRequests, resp.Status, resp.Message)

	// 锁定状态与账号是否存在无关：不存在的邮箱同样会被锁，
	// 因此"是否被锁"不能用来探测账号是否存在。
	ghostEmail := "ghost-" + uuid.NewString()[:8] + "@example.test"
	for i := 0; i < maxLoginFailures; i++ {
		resp = h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
			"tenant_slug": slug, "email": ghostEmail, "password": "wrong-password-123",
		}, "")
	}
	assert.Equal(t, http.StatusTooManyRequests, resp.Status, resp.Message)
	assert.Equal(t, "auth.too_many_attempts", resp.Code)

	// 把锁定时间拨到过去，验证解锁路径（不等真实 15 分钟）。
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.login_failures
		                        SET locked_until = now() - interval '1 minute'
		                        WHERE email = $1`, email)
		return err
	}))

	resp = h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": slug, "email": email, "password": testPassword,
	}, "")
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)

	// 成功登录清零：再失败 4 次仍是 401，不会继承之前的计数。
	for i := 0; i < maxLoginFailures-1; i++ {
		resp = wrong()
		require.Equal(t, http.StatusUnauthorized, resp.Status, fmt.Sprintf("清零后第 %d 次失败", i+1))
	}
}
