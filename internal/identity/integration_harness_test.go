package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

const (
	testPassword   = "Expensio-Integration-2026"
	testIssuer     = "expensio-test"
	testAccessTTL  = 15 * time.Minute
	testRefreshTTL = 24 * time.Hour
)

// testInternalToken 只用于测试服务间令牌；生产值来自环境变量 / K8s Secret。
const testInternalToken = "0123456789abcdef0123456789abcdef"

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

// harness 是一个完整的 identity 服务：真实 PostgreSQL + 应用角色 + 真实 HTTP 端口。
//
// 用应用角色（NOBYPASSRLS）连库是刻意的：换成一个能绕过 RLS 的账号，
// 下面的隔离测试会全部"通过"，但什么也没证明。
type harness struct {
	t        *testing.T
	db       *db.DB
	svc      *Service
	tokens   *auth.TokenIssuer
	server   *httptest.Server
	client   *http.Client
	notifier *recordingNotifier
}

// recordingNotifier 记录邀请邮件，避免测试依赖真实 notification 服务。
type recordingNotifier struct {
	mu     sync.Mutex
	emails []InvitationEmail
	tenant []uuid.UUID
	err    error
}

func (n *recordingNotifier) EnqueueInvitation(_ context.Context, tenantID uuid.UUID, in InvitationEmail) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.emails = append(n.emails, in)
	n.tenant = append(n.tenant, tenantID)
	return nil
}

func (n *recordingNotifier) last() (InvitationEmail, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.emails) == 0 {
		return InvitationEmail{}, false
	}
	return n.emails[len(n.emails)-1], true
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	pg := testenv.StartPostgres(t, "identity")
	database, err := db.Open(context.Background(), pg.AppDSN("identity"), 10)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	_, signKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tokens, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: testIssuer, PrivateKey: signKey,
		AccessTTL: testAccessTTL, RefreshTTL: testRefreshTTL,
	})
	require.NoError(t, err)

	notifier := &recordingNotifier{}
	svc, err := NewService(database, tokens, InvitationOptions{
		Notifier: notifier,
		BaseURL:  "http://invite.test",
		TTL:      24 * time.Hour,
	})
	require.NoError(t, err)

	handler := NewHandler(svc)
	handler.EnableInternalAPI(testInternalToken)

	engine := gin.New()
	RegisterRoutes(engine, handler, tokens)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &harness{
		t:        t,
		db:       database,
		svc:      svc,
		tokens:   tokens,
		server:   srv,
		client:   &http.Client{Timeout: 10 * time.Second},
		notifier: notifier,
	}
}

type apiResponse struct {
	Status  int
	Header  http.Header
	Code    string
	Message string
	Data    json.RawMessage
}

func (h *harness) call(method, path string, body any, token string) apiResponse {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(h.t, err)
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	require.NoError(h.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)

	out := apiResponse{Status: resp.StatusCode, Header: resp.Header}
	if len(raw) > 0 {
		var env struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		require.NoError(h.t, json.Unmarshal(raw, &env), "响应不是统一信封: %s", raw)
		out.Code, out.Message, out.Data = env.Code, env.Message, env.Data
	}
	return out
}

func (h *harness) register() Session {
	h.t.Helper()
	slug := uniqueSlug("acme")
	resp := h.call(http.MethodPost, "/api/v1/auth/register", map[string]any{
		"tenant_slug":  slug,
		"tenant_name":  "Tenant " + slug,
		"email":        "owner@" + slug + ".test",
		"password":     testPassword,
		"display_name": "Owner " + slug,
	}, "")
	require.Contains(h.t, []int{http.StatusOK, http.StatusCreated}, resp.Status)
	var s Session
	require.NoError(h.t, json.Unmarshal(resp.Data, &s))
	return s
}

func (h *harness) login(slug, email string) Session {
	h.t.Helper()
	resp := h.call(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"tenant_slug": slug,
		"email":       email,
		"password":    testPassword,
	}, "")
	require.Equal(h.t, http.StatusOK, resp.Status, resp.Message)
	var s Session
	require.NoError(h.t, json.Unmarshal(resp.Data, &s))
	return s
}

// seedMember 直接写库创建成员：当前没有"邀请成员"的 API，
// 但 RBAC 测试需要一个已知角色的账号，且必须走真实的 users / memberships 表。
func (h *harness) seedMember(tenantID uuid.UUID, role tenantctx.Role) Session {
	h.t.Helper()

	hash, err := auth.HashPassword(testPassword)
	require.NoError(h.t, err)

	userID := uuid.New()
	email := fmt.Sprintf("%s-%s@seed.test", role, userID.String()[:8])

	require.NoError(h.t, h.db.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			if err := NewRepository().CreateUser(ctx, tx, &User{
				ID: userID, Email: email, PasswordHash: hash,
				DisplayName: string(role), Status: "active",
			}); err != nil {
				return err
			}
			return NewRepository().CreateMembership(ctx, tx, tenantID, userID, string(role), "active", userID)
		}))

	slug := h.tenantSlug(tenantID)
	return h.login(slug, email)
}

func (h *harness) tenantSlug(tenantID uuid.UUID) string {
	h.t.Helper()
	var slug string
	require.NoError(h.t, h.db.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT slug FROM app.tenants WHERE id = $1`, tenantID).Scan(&slug)
	}))
	return slug
}

// tenantCtx 构造租户上下文，模拟 JWT 校验后的请求上下文。
func tenantCtx(t *testing.T, tenantID, userID uuid.UUID, role tenantctx.Role) context.Context {
	t.Helper()
	ctx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID: tenantID, UserID: userID, Role: role, Slug: "it",
	})
	require.NoError(t, err)
	return ctx
}

// uniqueSlug 每次生成新租户，测试之间互不干扰。
func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

// statusOf 把 apperr 转成 HTTP 状态码，供服务层测试断言。
func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return apperr.Status(err)
}
