package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
)

const testInternalSecret = "0123456789abcdef0123456789abcdef"

func TestContactsClientSignsTenantIntoBearerToken(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	var gotTenant uuid.UUID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/internal/tenant-contacts" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// 租户只能出现在签名令牌里，绝不能出现在 URL 或请求体。
		if r.URL.Query().Get("tenant_id") != "" || strings.Contains(r.URL.RawQuery, "tenant") {
			t.Errorf("tenant must not appear in query: %s", r.URL.RawQuery)
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var err error
		gotTenant, err = serviceauth.Verify(testInternalSecret, raw, time.Now())
		if err != nil {
			t.Errorf("invalid service token: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "ok",
			"data": []map[string]any{{
				"user_id": userID, "email": "user@example.com",
				"display_name": "User", "role": "admin", "status": "active",
			}},
		})
	}))
	defer srv.Close()

	client, err := NewContactsClient(srv.URL, testInternalSecret, 2*time.Second)
	require.NoError(t, err)
	contacts, err := client.TenantContacts(context.Background(), tenantID)
	require.NoError(t, err)

	assert.Equal(t, tenantID, gotTenant)
	require.Len(t, contacts, 1)
	assert.Equal(t, userID, contacts[0].UserID)
	assert.Equal(t, "user@example.com", contacts[0].Email)
	assert.True(t, contacts[0].IsManager())
}

func TestContactsClientDoesNotLeakErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal","message":"secret@example.com"}`))
	}))
	defer srv.Close()

	client, err := NewContactsClient(srv.URL, testInternalSecret, 2*time.Second)
	require.NoError(t, err)
	_, err = client.TenantContacts(context.Background(), uuid.New())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret@example.com")
	assert.Contains(t, err.Error(), "500")
}

func TestContactsClientRejectsWeakSecret(t *testing.T) {
	_, err := NewContactsClient("http://127.0.0.1:8081", "short", time.Second)
	require.Error(t, err)
}
