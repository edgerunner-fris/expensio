package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
)

const testInternalToken = "0123456789abcdef0123456789abcdef"

// denyParser 只用于满足 RegisterRoutes 的签名；本测试不调用用户接口。
type denyParser struct{}

func (denyParser) ParseAccess(string) (*auth.Claims, error) {
	return nil, errors.New("测试中不校验用户令牌")
}

func newInternalEmailServer(t *testing.T, database *db.DB) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	handler := NewHandler(NewService(database))
	handler.EnableInternalAPI(testInternalToken)
	engine := gin.New()
	RegisterRoutes(engine, handler, denyParser{})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv
}

func postInternalEmail(t *testing.T, srv *httptest.Server, token string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/internal/emails", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestInternalEmailRequiresSignedTenantToken(t *testing.T) {
	database := newNotificationDB(t)
	srv := newInternalEmailServer(t, database)
	tenantID := uuid.New()

	payload := map[string]string{
		"recipient":  "invitee@example.com",
		"subject":    "邀请",
		"body":       "正文",
		"dedupe_key": "invitation:1",
	}

	assert.Equal(t, http.StatusUnauthorized, postInternalEmail(t, srv, "", payload).StatusCode)
	assert.Equal(t, http.StatusUnauthorized,
		postInternalEmail(t, srv, "not-a-signed-token", payload).StatusCode,
		"任意字符串不能被当作内部令牌")

	signed, err := serviceauth.Sign(testInternalToken, tenantID, time.Minute, time.Now())
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		postInternalEmail(t, srv, signed+"x", payload).StatusCode,
		"篡改签名必须失败")
}

func TestInternalEmailQueuesUnderTenantFromToken(t *testing.T) {
	database := newNotificationDB(t)
	srv := newInternalEmailServer(t, database)
	tenantA, tenantB := uuid.New(), uuid.New()

	sign := func(tenantID uuid.UUID) string {
		token, err := serviceauth.Sign(testInternalToken, tenantID, time.Minute, time.Now())
		require.NoError(t, err)
		return token
	}
	payload := map[string]string{
		"recipient":  "invitee@example.com",
		"subject":    "加入 Acme 的 Expensio 团队",
		"body":       "点击链接加入",
		"dedupe_key": "invitation:abc",
	}

	resp := postInternalEmail(t, srv, sign(tenantA), payload)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var (
		recipient, subject, dedupeKey, status string
	)
	require.NoError(t, database.WithinTenantID(context.Background(), tenantA,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT recipient, subject, dedupe_key, status FROM app.email_outbox`).
				Scan(&recipient, &subject, &dedupeKey, &status)
		}))
	assert.Equal(t, "invitee@example.com", recipient)
	assert.Equal(t, "加入 Acme 的 Expensio 团队", subject)
	assert.Equal(t, "invitation:abc", dedupeKey)
	assert.Equal(t, "pending", status)

	// 同一租户、同一幂等键重复投递只保留一行。
	assert.Equal(t, http.StatusCreated, postInternalEmail(t, srv, sign(tenantA), payload).StatusCode)
	var countA int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantA,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.email_outbox`).Scan(&countA)
		}))
	assert.Equal(t, 1, countA)

	// 相同幂等键在另一个租户是另一封信；RLS 让两边互相看不到。
	assert.Equal(t, http.StatusCreated, postInternalEmail(t, srv, sign(tenantB), payload).StatusCode)
	var countB int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantB,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.email_outbox`).Scan(&countB)
		}))
	assert.Equal(t, 1, countB)
	require.NoError(t, database.WithinTenantID(context.Background(), tenantA,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM app.email_outbox`).Scan(&countA)
		}))
	assert.Equal(t, 1, countA)
}

func TestInternalEmailValidatesPayload(t *testing.T) {
	database := newNotificationDB(t)
	srv := newInternalEmailServer(t, database)
	token, err := serviceauth.Sign(testInternalToken, uuid.New(), time.Minute, time.Now())
	require.NoError(t, err)

	cases := []map[string]string{
		{"recipient": "not-an-email", "subject": "s", "body": "b", "dedupe_key": "k"},
		{"recipient": "a@example.com", "subject": "  ", "body": "b", "dedupe_key": "k"},
		{"recipient": "a@example.com", "subject": "s", "body": "b", "dedupe_key": ""},
	}
	for _, payload := range cases {
		resp := postInternalEmail(t, srv, token, payload)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "payload=%v", payload)
	}

	// 非 JSON 请求体同样是 400，而不是 500。
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/internal/emails",
		bytes.NewReader([]byte("{")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
