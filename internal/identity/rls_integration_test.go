package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// TestRLSIsEnforced 用真实的 NOBYPASSRLS 应用角色验证租户隔离。
// 这些断言不经过服务层：服务层可能"碰巧"自己加了 WHERE tenant_id，从而掩盖 RLS 没生效。
func TestRLSIsEnforced(t *testing.T) {
	h := newHarness(t)

	acme := h.register()
	beta := h.register()
	require.NotEqual(t, acme.Profile.TenantID, beta.Profile.TenantID)

	t.Run("应用角色既不超级用户也不绕过RLS", func(t *testing.T) {
		var (
			role      string
			superuser bool
			bypassRLS bool
		)
		require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT current_user, rolsuper, rolbypassrls
				FROM pg_roles WHERE rolname = current_user`).Scan(&role, &superuser, &bypassRLS)
		}))
		assert.Equal(t, "identity_app", role)
		assert.False(t, superuser, "应用账号不能是超级用户")
		assert.False(t, bypassRLS, "应用账号一旦 BYPASSRLS，租户隔离就形同虚设")
	})

	t.Run("没有租户上下文时查不到任何成员", func(t *testing.T) {
		var n int
		require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.memberships`).Scan(&n)
		}))
		assert.Zero(t, n, "RLS 在 current_setting 为空时必须返回 0 行")
	})

	t.Run("各租户只看到自己的成员", func(t *testing.T) {
		acmeCtx := tenantCtx(t, acme.Profile.TenantID, acme.Profile.UserID, tenantctx.RoleOwner)
		betaCtx := tenantCtx(t, beta.Profile.TenantID, beta.Profile.UserID, tenantctx.RoleOwner)

		var acmeMembers, betaMembers []Member
		require.NoError(t, h.db.WithinTenant(acmeCtx, func(ctx context.Context, tx pgx.Tx) error {
			var total int
			var err error
			acmeMembers, total, err = NewRepository().ListMembers(ctx, tx, 50, 0)
			assert.Equal(t, len(acmeMembers), total)
			return err
		}))
		require.NoError(t, h.db.WithinTenant(betaCtx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			betaMembers, _, err = NewRepository().ListMembers(ctx, tx, 50, 0)
			return err
		}))

		require.Len(t, acmeMembers, 1)
		require.Len(t, betaMembers, 1)
		assert.Equal(t, acme.Profile.UserID, acmeMembers[0].UserID)
		assert.Equal(t, beta.Profile.UserID, betaMembers[0].UserID)
		assert.NotEqual(t, acmeMembers[0].UserID, betaMembers[0].UserID)
	})

	t.Run("跨租户按ID读取返回空", func(t *testing.T) {
		acmeCtx := tenantCtx(t, acme.Profile.TenantID, acme.Profile.UserID, tenantctx.RoleOwner)

		var got *Member
		err := h.db.WithinTenant(acmeCtx, func(ctx context.Context, tx pgx.Tx) error {
			m, err := NewRepository().MemberByID(ctx, tx, beta.Profile.UserID)
			got = m
			return err
		})
		assert.True(t, IsNoRows(err), "acme 的上下文读 beta 的成员必须查不到，实际: %v", err)
		assert.Nil(t, got)
	})

	t.Run("跨租户写入被策略拒绝", func(t *testing.T) {
		newUser := uuid.New()

		err := h.db.WithinTenantID(context.Background(), acme.Profile.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			// user 表本身不受 RLS 约束（账号是全局的），但 membership 必须落在当前租户。
			if err := NewRepository().CreateUser(ctx, tx, &User{
				ID: newUser, Email: newUser.String() + "@evil.test",
				PasswordHash: "x", DisplayName: "evil", Status: "active",
			}); err != nil {
				return err
			}
			return NewRepository().CreateMembership(ctx, tx, beta.Profile.TenantID,
				newUser, "viewer", "active", acme.Profile.UserID)
		})

		require.Error(t, err, "把成员写进别的租户必须失败")
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr), "应返回 PostgreSQL 错误，实际: %v", err)
		assert.Equal(t, "42501", pgErr.Code, "应为 row-level security 违规")
	})

	t.Run("审计日志同样按租户隔离", func(t *testing.T) {
		acmeCtx := tenantCtx(t, acme.Profile.TenantID, acme.Profile.UserID, tenantctx.RoleOwner)
		betaCtx := tenantCtx(t, beta.Profile.TenantID, beta.Profile.UserID, tenantctx.RoleOwner)

		acmeLogs, betaLogs := listAudit(t, h, acmeCtx), listAudit(t, h, betaCtx)
		require.NotEmpty(t, acmeLogs)
		require.NotEmpty(t, betaLogs)

		betaIDs := make(map[uuid.UUID]struct{}, len(betaLogs))
		for _, l := range betaLogs {
			betaIDs[l.ID] = struct{}{}
		}
		for _, l := range acmeLogs {
			_, leaked := betaIDs[l.ID]
			assert.False(t, leaked, "acme 的审计结果里出现了 beta 的记录")
		}

		var global int
		require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.audit_logs`).Scan(&global)
		}))
		assert.Zero(t, global, "无租户上下文时审计表也必须为 0 行")
	})

	t.Run("事务结束后租户上下文不残留", func(t *testing.T) {
		acmeCtx := tenantCtx(t, acme.Profile.TenantID, acme.Profile.UserID, tenantctx.RoleOwner)
		var n int
		require.NoError(t, h.db.WithinTenant(acmeCtx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.memberships`).Scan(&n)
		}))
		require.Equal(t, 1, n)

		// set_config(..., true) 是事务级的；连接归还池后不能把租户带给下一个请求。
		require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.memberships`).Scan(&n)
		}))
		assert.Zero(t, n)
	})

	t.Run("缺少租户上下文时WithinTenant直接报错", func(t *testing.T) {
		err := h.db.WithinTenant(context.Background(), func(context.Context, pgx.Tx) error {
			t.Fatal("不应该执行到事务体")
			return nil
		})
		assert.Error(t, err, "没有上下文时必须报错，而不是退回全表可读")
	})

	t.Run("空租户ID被拒绝", func(t *testing.T) {
		err := h.db.WithinTenantID(context.Background(), uuid.Nil, func(context.Context, pgx.Tx) error {
			t.Fatal("不应该执行到事务体")
			return nil
		})
		assert.Error(t, err)
	})
}

func listAudit(t *testing.T, h *harness, ctx context.Context) []AuditEntry {
	t.Helper()
	var entries []AuditEntry
	require.NoError(t, h.db.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entries, _, err = NewRepository().ListAudit(ctx, tx, 100, 0)
		return err
	}))
	return entries
}
