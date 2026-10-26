package identity

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

var testMeta = RequestMeta{IP: "127.0.0.1", UserAgent: "integration-test"}

func TestRefreshRotationAndReplay(t *testing.T) {
	h := newHarness(t)
	session := h.register()

	r1, err := h.svc.Refresh(context.Background(), session.RefreshToken, testMeta)
	require.NoError(t, err)
	require.NotEmpty(t, r1.RefreshToken)
	assert.NotEqual(t, session.RefreshToken, r1.RefreshToken, "刷新必须轮换，而不是原样返回")

	// 旧令牌再用一次：判定为重放，整族撤销。
	_, err = h.svc.Refresh(context.Background(), session.RefreshToken, testMeta)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))

	// 攻击者拿到的"新令牌"也必须失效——否则撤销族只是换了个名字。
	_, err = h.svc.Refresh(context.Background(), r1.RefreshToken, testMeta)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))

	// 审计只记事实，不记令牌本身。
	logs := listAudit(t, h, tenantCtx(t, session.Profile.TenantID, session.Profile.UserID, tenantctx.RoleOwner))
	var replayed int
	for _, l := range logs {
		if l.Action == "auth.refresh_replayed" {
			replayed++
		}
		assert.NotContains(t, l.Metadata, "token")
		assert.NotContains(t, l.Metadata, "refresh_token")
	}
	assert.GreaterOrEqual(t, replayed, 1, "重放必须留下审计记录")
}

func TestConcurrentRefreshOnlyOneWins(t *testing.T) {
	h := newHarness(t)
	session := h.register()

	const workers = 8
	type result struct {
		session *Session
		err     error
	}
	results := make([]result, workers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让竞争同时发生，而不是被调度顺序错开
			s, err := h.svc.Refresh(context.Background(), session.RefreshToken, testMeta)
			results[i] = result{session: s, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		successes int
		winner    *Session
	)
	for _, r := range results {
		if r.err == nil {
			successes++
			winner = r.session
			continue
		}
		assert.Equal(t, http.StatusUnauthorized, statusOf(r.err), "失败的刷新只能是 401，实际: %v", r.err)
	}
	assert.Equal(t, 1, successes, "同一个刷新令牌只能被成功消费一次")

	// 竞争中的失败者按"重放"处理，会撤销整族；赢得竞争的那张新令牌同样失效。
	// 这是有意的取舍：宁可让用户重新登录，也不让可能泄露的令牌继续可用。
	require.NotNil(t, winner)
	_, err := h.svc.Refresh(context.Background(), winner.RefreshToken, testMeta)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))

	var active int
	require.NoError(t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM app.refresh_tokens
			WHERE user_id = $1 AND revoked_at IS NULL`, session.Profile.UserID).Scan(&active)
	}))
	assert.Zero(t, active, "整族都应为已撤销")
}

func TestLogoutRevokesSession(t *testing.T) {
	h := newHarness(t)
	session := h.register()

	require.NoError(t, h.svc.Logout(context.Background(), session.RefreshToken, testMeta))

	_, err := h.svc.Refresh(context.Background(), session.RefreshToken, testMeta)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, statusOf(err))

	logs := listAudit(t, h, tenantCtx(t, session.Profile.TenantID, session.Profile.UserID, tenantctx.RoleOwner))
	var loggedOut bool
	for _, l := range logs {
		if l.Action == "auth.logout" {
			loggedOut = true
		}
	}
	assert.True(t, loggedOut, "登出必须留下审计记录")
}

func TestRefreshRejectsUnknownAndEmptyToken(t *testing.T) {
	h := newHarness(t)

	// 空串是客户端构造错误（请求缺字段），归入 400；
	// 其余"看起来像令牌但查不到"一律 401，不区分具体原因。
	for name, tc := range map[string]struct {
		token  string
		status int
	}{
		"空串":    {"", http.StatusBadRequest},
		"随机串":   {"not-a-real-refresh-token", http.StatusUnauthorized},
		"超长随机串": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.Refresh(context.Background(), tc.token, testMeta)
			require.Error(t, err)
			assert.Equal(t, tc.status, statusOf(err))
		})
	}
}

// TestTenantMembershipIsRequired 确认"账号存在"不等于"能进入该租户"。
func TestTenantMembershipIsRequired(t *testing.T) {
	h := newHarness(t)
	acme := h.register()
	beta := h.register()

	// 用 acme 的邮箱去登录 beta 租户：账号存在，但不是 beta 的成员。
	resp := h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": h.tenantSlug(beta.Profile.TenantID),
		"email":       acme.Profile.Email,
		"password":    testPassword,
	}, "")
	assert.Equal(t, http.StatusUnauthorized, resp.Status)
	assert.Equal(t, "auth.unauthorized", resp.Code, "文案不能暴露'账号存在但不在该租户'")
}
