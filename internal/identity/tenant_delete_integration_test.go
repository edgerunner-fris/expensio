package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestDeleteTenant(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	tenantA := owner.Profile.TenantID
	slugA := h.tenantSlug(tenantA)

	// 另一个租户：验证删除后不能再切回来。
	tenantB := h.createTenant("beta")
	h.addMembership(tenantB, owner.Profile.UserID, "owner")
	resp := h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantB.String()}, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var sessionB Session
	require.NoError(t, json.Unmarshal(resp.Data, &sessionB))

	// 非所有者（admin）不能删除。
	admin := h.seedMember(tenantA, tenantctx.RoleAdmin)
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current",
		map[string]any{"slug": slugA}, admin.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status, resp.Message)

	// 没有确认体 / 确认体不匹配：400，且什么都不改。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current", nil, owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status, resp.Message)
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current",
		map[string]any{"slug": "not-" + slugA}, owner.AccessToken)
	assert.Equal(t, http.StatusBadRequest, resp.Status, resp.Message)

	var deletedAt *time.Time
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT deleted_at FROM app.tenants WHERE id = $1`, tenantA).Scan(&deletedAt)
	}))
	require.Nil(t, deletedAt, "确认失败时租户不能被删除")

	// 留一条待接受邀请，删除后必须作废。
	invitation := h.call(http.MethodPost, "/api/v1/tenants/current/invitations", map[string]any{
		"email": "pending-" + uuid.NewString()[:8] + "@example.test",
		"role":  "member",
	}, owner.AccessToken)
	require.Equal(t, http.StatusCreated, invitation.Status, invitation.Message)

	// 所有者用正确 slug 确认：删除成功。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current",
		map[string]any{"slug": slugA}, owner.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var out DeleteTenantResult
	require.NoError(t, json.Unmarshal(resp.Data, &out))
	assert.GreaterOrEqual(t, out.SessionsRevoked, int64(1))

	// 刷新令牌立即失效。
	refresh := h.call(http.MethodPost, "/api/v1/auth/refresh",
		map[string]any{"refresh_token": owner.RefreshToken}, "")
	assert.Equal(t, http.StatusUnauthorized, refresh.Status, refresh.Message)

	// 用同一个邮箱和 slug 重新登录：租户已被过滤，凭据无效。
	login := h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": slugA,
		"email":       owner.Profile.Email,
		"password":    testPassword,
	}, "")
	assert.Equal(t, http.StatusUnauthorized, login.Status, login.Message)

	// 从另一个租户切回已删除的租户：403。
	resp = h.call(http.MethodPost, "/api/v1/auth/switch-tenant",
		map[string]any{"tenant_id": tenantA.String()}, sessionB.AccessToken)
	assert.Equal(t, http.StatusForbidden, resp.Status, resp.Message)

	// 租户列表里不再出现已删除的租户。
	resp = h.call(http.MethodGet, "/api/v1/auth/tenants", nil, sessionB.AccessToken)
	require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	var list struct {
		Items []TenantMembership `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Data, &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, tenantB, list.Items[0].TenantID)

	// 数据库状态：租户与成员关系软删除、邀请作废、审计留下最后一条。
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT deleted_at FROM app.tenants WHERE id = $1`, tenantA).Scan(&deletedAt)
	}))
	assert.NotNil(t, deletedAt)

	var liveMembers, liveInvitations, activeTokens int
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.memberships WHERE tenant_id = $1 AND deleted_at IS NULL`,
			tenantA).Scan(&liveMembers); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.invitations
			 WHERE tenant_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`,
			tenantA).Scan(&liveInvitations); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM app.refresh_tokens WHERE tenant_id = $1 AND revoked_at IS NULL`,
			tenantA).Scan(&activeTokens)
	}))
	assert.Zero(t, liveMembers)
	assert.Zero(t, liveInvitations)
	assert.Zero(t, activeTokens)
	assert.Equal(t, 1, h.auditCount(tenantA, "tenant.deleted"))

	// 再次删除：租户已不可见，返回 404。
	resp = h.call(http.MethodDelete, "/api/v1/tenants/current",
		map[string]any{"slug": slugA}, owner.AccessToken)
	assert.Equal(t, http.StatusNotFound, resp.Status, resp.Message)
}
