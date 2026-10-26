package identity

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// TestRBACMatrix 用四种角色跑同一组端点，确认权限点在 HTTP 层而不是"前端隐藏按钮"。
func TestRBACMatrix(t *testing.T) {
	h := newHarness(t)
	owner := h.register()
	tenantID := owner.Profile.TenantID

	admin := h.seedMember(tenantID, tenantctx.RoleAdmin)
	member := h.seedMember(tenantID, tenantctx.RoleMember)
	viewer := h.seedMember(tenantID, tenantctx.RoleViewer)

	roles := []struct {
		name string
		s    Session
	}{
		{"owner", owner},
		{"admin", admin},
		{"member", member},
		{"viewer", viewer},
	}

	t.Run("me 对所有角色开放", func(t *testing.T) {
		for _, r := range roles {
			resp := h.call(http.MethodGet, "/api/v1/me", nil, r.s.AccessToken)
			assert.Equal(t, http.StatusOK, resp.Status, "%s 读取 /me 失败: %s", r.name, resp.Message)
		}
	})

	for _, path := range []string{"/api/v1/tenants/current/members", "/api/v1/audit-logs"} {
		t.Run("管理视图 "+path, func(t *testing.T) {
			for _, r := range roles {
				resp := h.call(http.MethodGet, path, nil, r.s.AccessToken)
				switch r.name {
				case "owner", "admin":
					assert.Equal(t, http.StatusOK, resp.Status, "%s 应可访问: %s", r.name, resp.Message)
				default:
					assert.Equal(t, http.StatusForbidden, resp.Status, "%s 不应可访问", r.name)
					assert.Equal(t, "auth.forbidden", resp.Code)
				}
			}
		})
	}

	t.Run("无令牌与坏令牌一律401", func(t *testing.T) {
		for _, token := range []string{"", "not-a-jwt"} {
			resp := h.call(http.MethodGet, "/api/v1/me", nil, token)
			assert.Equal(t, http.StatusUnauthorized, resp.Status)
			assert.Equal(t, "auth.unauthorized", resp.Code)
		}
	})

	t.Run("管理员可以调整非owner成员角色", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+viewer.Profile.UserID.String(),
			map[string]string{"role": "member"}, admin.AccessToken)
		require.Equal(t, http.StatusOK, resp.Status, resp.Message)

		var updated Member
		require.NoError(t, json.Unmarshal(resp.Data, &updated))
		assert.Equal(t, "member", updated.Role)

		// 改回去，避免影响同一租户里其他断言对"viewer 仍是 viewer"的假设。
		resp = h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+viewer.Profile.UserID.String(),
			map[string]string{"role": "viewer"}, owner.AccessToken)
		require.Equal(t, http.StatusOK, resp.Status, resp.Message)
	})

	t.Run("只有owner能修改owner", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+owner.Profile.UserID.String(),
			map[string]string{"role": "admin"}, admin.AccessToken)
		assert.Equal(t, http.StatusForbidden, resp.Status)
		assert.Equal(t, "auth.forbidden", resp.Code)
	})

	t.Run("不能修改自己的角色", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+owner.Profile.UserID.String(),
			map[string]string{"role": "owner"}, owner.AccessToken)
		assert.Equal(t, http.StatusConflict, resp.Status)
		assert.Equal(t, "member.self_change", resp.Code)
	})

	t.Run("不存在成员返回404", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+uuid.NewString(),
			map[string]string{"role": "member"}, owner.AccessToken)
		assert.Equal(t, http.StatusNotFound, resp.Status)
	})

	t.Run("非法角色返回400", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+member.Profile.UserID.String(),
			map[string]string{"role": "superuser"}, owner.AccessToken)
		assert.Equal(t, http.StatusBadRequest, resp.Status)
	})

	t.Run("普通成员不能改别人的角色", func(t *testing.T) {
		resp := h.call(http.MethodPatch,
			"/api/v1/tenants/current/members/"+viewer.Profile.UserID.String(),
			map[string]string{"role": "admin"}, member.AccessToken)
		assert.Equal(t, http.StatusForbidden, resp.Status)
	})

	// 成员列表按租户隔离：两个租户的 token 各看各的，且响应里不出现对方的邮箱。
	t.Run("成员列表不跨租户", func(t *testing.T) {
		other := h.register()
		resp := h.call(http.MethodGet, "/api/v1/tenants/current/members", nil, other.AccessToken)
		require.Equal(t, http.StatusOK, resp.Status, resp.Message)

		var page struct {
			Items []Member `json:"items"`
			Total int64    `json:"total"`
		}
		require.NoError(t, json.Unmarshal(resp.Data, &page))
		assert.Equal(t, int64(1), page.Total)
		require.Len(t, page.Items, 1)
		assert.Equal(t, other.Profile.Email, page.Items[0].Email)
		assert.NotContains(t, string(resp.Data), owner.Profile.Email)
	})

	t.Run("审计记录包含角色变更且不含敏感信息", func(t *testing.T) {
		resp := h.call(http.MethodGet, "/api/v1/audit-logs?page_size=100", nil, owner.AccessToken)
		require.Equal(t, http.StatusOK, resp.Status, resp.Message)

		var page struct {
			Items []AuditEntry `json:"items"`
		}
		require.NoError(t, json.Unmarshal(resp.Data, &page))

		var actions []string
		for _, e := range page.Items {
			actions = append(actions, e.Action)
			if e.Action == "member.role_updated" {
				assert.NotContains(t, e.Metadata, "password")
				assert.NotContains(t, e.Metadata, "token")
			}
		}
		joined := strings.Join(actions, ",")
		assert.Contains(t, joined, "member.role_updated")
		assert.Contains(t, joined, "auth.login")
	})
}
