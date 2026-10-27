package expense

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// newHTTPTestServerWithAttachments 与 newHTTPTestServer 相同，但挂上真实对象存储。
func newHTTPTestServerWithAttachments(t *testing.T, database *db.DB) (*gin.Engine, *auth.TokenIssuer) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	_, signKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuer, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: "expensio-test", PrivateKey: signKey,
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour,
	})
	require.NoError(t, err)

	engine := gin.New()
	RegisterRoutes(engine,
		NewHandlerWithAttachments(newService(t, database), newAttachmentService(t, database)), issuer)
	return engine, issuer
}

func TestHTTPAttachmentsAndCollaboration(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServerWithAttachments(t, database)
	tenant, user := uuid.New(), uuid.New()
	token := tokenFor(t, issuer, tenant, user, tenantctx.RoleMember)
	adminToken := tokenFor(t, issuer, tenant, uuid.New(), tenantctx.RoleAdmin)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/expenses", token, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[expenseView](t, rec)
	base := "/api/v1/expenses/" + created.ID.String()

	// ---- 附件：presign → 直传 → confirm → 列表 → 下载 ----
	body := []byte("%PDF-1.7 fake invoice")
	rec = doRequest(t, engine, http.MethodPost, base+"/attachments/presign", token, map[string]any{
		"file_name": "invoice.pdf", "content_type": "application/pdf", "size_bytes": len(body),
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	up := decode[PresignUpload](t, rec)
	require.Contains(t, up.ObjectKey, "tenants/"+tenant.String())

	putObject(t, &up, body)

	rec = doRequest(t, engine, http.MethodPost, base+"/attachments", token, map[string]any{
		"object_key": up.ObjectKey, "file_name": "invoice.pdf",
		"content_type": "application/pdf", "size_bytes": len(body),
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	confirmed := decode[struct {
		ID       uuid.UUID `json:"id"`
		FileName string    `json:"file_name"`
	}](t, rec)
	assert.Equal(t, "invoice.pdf", confirmed.FileName)

	rec = doRequest(t, engine, http.MethodGet, base+"/attachments", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	listed := decode[struct {
		Items []AttachmentView `json:"items"`
	}](t, rec)
	require.Len(t, listed.Items, 1)
	assert.NotEmpty(t, listed.Items[0].DownloadURL)

	rec = doRequest(t, engine, http.MethodDelete,
		base+"/attachments/"+confirmed.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// ---- 评论 ----
	rec = doRequest(t, engine, http.MethodPost, base+"/comments", adminToken,
		map[string]any{"body": "报销标准请参考差旅制度"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	comment := decode[struct {
		ID   uuid.UUID `json:"id"`
		Body string    `json:"body"`
	}](t, rec)
	assert.Equal(t, "报销标准请参考差旅制度", comment.Body)

	rec = doRequest(t, engine, http.MethodGet, base+"/comments", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	comments := decode[struct {
		Items []commentView `json:"items"`
	}](t, rec)
	require.Len(t, comments.Items, 1)

	rec = doRequest(t, engine, http.MethodDelete,
		base+"/comments/"+comment.ID.String(), adminToken, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// ---- 审批进度：默认单级 ----
	rec = doRequest(t, engine, http.MethodGet, base+"/approvals", token, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	progress := decode[struct {
		RequiredLevels int16          `json:"required_levels"`
		ApprovedLevels int16          `json:"approved_levels"`
		Items          []approvalView `json:"items"`
	}](t, rec)
	assert.Equal(t, int16(1), progress.RequiredLevels)
	assert.Zero(t, progress.ApprovedLevels)
	assert.Empty(t, progress.Items)

	// ---- 审批策略：管理岗可写，普通成员 403 ----
	rec = doRequest(t, engine, http.MethodPut, "/api/v1/approval-policies/USD", adminToken,
		map[string]any{"second_level_threshold_minor": 500000})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodGet, "/api/v1/approval-policies", adminToken, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	policies := decode[struct {
		Items []policyView `json:"items"`
	}](t, rec)
	require.Len(t, policies.Items, 1)
	assert.Equal(t, "USD", policies.Items[0].Currency)

	rec = doRequest(t, engine, http.MethodPut, "/api/v1/approval-policies/EUR", token,
		map[string]any{"second_level_threshold_minor": 100})
	require.Equal(t, http.StatusForbidden, rec.Code)

	rec = doRequest(t, engine, http.MethodDelete, "/api/v1/approval-policies/USD", adminToken, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestHTTPAttachmentsWithoutStore(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database) // 不挂对象存储
	tenant, user := uuid.New(), uuid.New()
	token := tokenFor(t, issuer, tenant, user, tenantctx.RoleMember)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/expenses", token, validInput())
	require.Equal(t, http.StatusCreated, rec.Code)
	created := decode[expenseView](t, rec)

	rec = doRequest(t, engine, http.MethodGet,
		"/api/v1/expenses/"+created.ID.String()+"/attachments", token, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, CodeAttachmentStorage, decodeError(t, rec).Code)
}
