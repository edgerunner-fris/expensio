package expense

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/objectstore"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// newAttachmentService 连接真实 MinIO。附件链路的关键风险（签名头、
// 对象键、Stat 语义）只有在真实 S3 兼容实现上才能验证。
func newAttachmentService(t *testing.T, database *db.DB) *AttachmentService {
	t.Helper()
	m := testenv.StartMinIO(t)
	store, err := objectstore.New(context.Background(), objectstore.Config{
		Endpoint:  m.Endpoint(),
		AccessKey: m.AccessKey,
		SecretKey: m.SecretKey,
		Bucket:    m.Bucket,
	})
	require.NoError(t, err)
	return NewAttachmentService(database, store, zap.NewNop())
}

// putObject 按 presign 返回的指令上传，模拟浏览器直传。
func putObject(t *testing.T, up *PresignUpload, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, up.UploadURL, bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "直传失败: %s", string(raw))
}

func httpStatus(err error) int {
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return appErr.Status()
	}
	return 0
}

func TestAttachmentLifecycle(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	attachments := newAttachmentService(t, database)

	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	actor := Actor{UserID: user, Role: tenantctx.RoleMember}

	e, err := svc.CreateDraft(ctx, actor, validInput())
	require.NoError(t, err)

	body := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	up, err := attachments.PresignUpload(ctx, actor, e.ID, PresignAttachmentInput{
		FileName: "receipt.png", ContentType: "image/png", SizeBytes: int64(len(body)),
	})
	require.NoError(t, err)
	assert.Contains(t, up.ObjectKey, "tenants/"+tenant.String()+"/expenses/"+e.ID.String()+"/")
	assert.Equal(t, "PUT", up.Method)
	assert.Contains(t, up.Headers["Content-Type"], "image/png")

	// 确认之前对象还不存在，必须明确拒绝。
	_, err = attachments.ConfirmUpload(ctx, actor, e.ID, ConfirmAttachmentInput{
		ObjectKey: up.ObjectKey, FileName: "receipt.png",
		ContentType: "image/png", SizeBytes: int64(len(body)),
	})
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))

	putObject(t, up, body)

	a, err := attachments.ConfirmUpload(ctx, actor, e.ID, ConfirmAttachmentInput{
		ObjectKey: up.ObjectKey, FileName: "receipt.png",
		ContentType: "image/png", SizeBytes: int64(len(body)),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), a.SizeBytes)

	items, err := attachments.List(ctx, actor, e.ID)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "receipt.png", items[0].FileName)

	resp, err := http.Get(items[0].DownloadURL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, body, got, "下载内容必须与上传一致")
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "receipt.png",
		"下载必须带原始文件名，而不是对象键")
	downloadURL := items[0].DownloadURL

	require.NoError(t, attachments.Delete(ctx, actor, e.ID, a.ID))
	items, err = attachments.List(ctx, actor, e.ID)
	require.NoError(t, err)
	assert.Empty(t, items)

	// 元数据删除后对象也应被删除：同一个下载 URL 必须 404。
	gone, err := http.Get(downloadURL)
	require.NoError(t, err)
	defer gone.Body.Close()
	assert.Equal(t, http.StatusNotFound, gone.StatusCode)
}

func TestAttachmentRejectsInvalidMetadata(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	attachments := newAttachmentService(t, database)

	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	actor := Actor{UserID: user, Role: tenantctx.RoleMember}
	e, err := svc.CreateDraft(ctx, actor, validInput())
	require.NoError(t, err)

	cases := []struct {
		name string
		in   PresignAttachmentInput
	}{
		{"非白名单类型", PresignAttachmentInput{FileName: "a.exe", ContentType: "application/x-msdownload", SizeBytes: 10}},
		{"超过 10 MiB", PresignAttachmentInput{FileName: "a.pdf", ContentType: "application/pdf", SizeBytes: MaxAttachmentBytes + 1}},
		{"空大小", PresignAttachmentInput{FileName: "a.pdf", ContentType: "application/pdf", SizeBytes: 0}},
		{"路径穿越文件名", PresignAttachmentInput{FileName: "../a.pdf", ContentType: "application/pdf", SizeBytes: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := attachments.PresignUpload(ctx, actor, e.ID, tc.in)
			require.Error(t, err)
			assert.Equal(t, http.StatusBadRequest, httpStatus(err))
		})
	}
}

func TestAttachmentAuthorizeAndLimits(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	attachments := newAttachmentService(t, database)

	tenant, owner, other := uuid.New(), uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, owner, tenantctx.RoleMember)
	ownerActor := Actor{UserID: owner, Role: tenantctx.RoleMember}
	otherActor := Actor{UserID: other, Role: tenantctx.RoleMember}

	e, err := svc.CreateDraft(ctx, ownerActor, validInput())
	require.NoError(t, err)

	in := PresignAttachmentInput{FileName: "a.pdf", ContentType: "application/pdf", SizeBytes: 10}
	_, err = attachments.PresignUpload(ctx, otherActor, e.ID, in)
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "非提交人不能上传")

	// 占满 10 个名额后，第 11 个必须在签发阶段就失败。
	require.NoError(t, database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < MaxAttachmentsPerExpense; i++ {
			_, err := tx.Exec(ctx, `
				INSERT INTO app.expense_attachments
				    (id, tenant_id, expense_id, object_key, file_name, content_type, size_bytes, uploaded_by)
				VALUES ($1, $2, $3, $4, $5, 'application/pdf', 10, $6)`,
				uuid.New(), tenant, e.ID,
				fmt.Sprintf("tenants/%s/expenses/%s/%s.pdf", tenant, e.ID, uuid.New()),
				fmt.Sprintf("f%d.pdf", i), owner)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	_, err = attachments.PresignUpload(ctx, ownerActor, e.ID, in)
	require.Error(t, err)
	assert.Equal(t, http.StatusConflict, httpStatus(err))

	// 提交后附件冻结：草稿阶段是唯一可写窗口。
	_, err = svc.ApplyAction(ctx, ownerActor, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = attachments.PresignUpload(ctx, ownerActor, e.ID, in)
	require.Error(t, err)
	assert.Equal(t, http.StatusConflict, httpStatus(err))
}

func TestAttachmentRejectsForeignObjectKey(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	attachments := newAttachmentService(t, database)

	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	actor := Actor{UserID: user, Role: tenantctx.RoleMember}
	e, err := svc.CreateDraft(ctx, actor, validInput())
	require.NoError(t, err)

	foreign := fmt.Sprintf("tenants/%s/expenses/%s/%s.pdf", tenant, uuid.New(), uuid.New())
	_, err = attachments.ConfirmUpload(ctx, actor, e.ID, ConfirmAttachmentInput{
		ObjectKey: foreign, FileName: "x.pdf", ContentType: "application/pdf", SizeBytes: 10,
	})
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))

	// 另一租户的前缀同样不行。
	otherTenant := fmt.Sprintf("tenants/%s/expenses/%s/%s.pdf", uuid.New(), e.ID, uuid.New())
	_, err = attachments.ConfirmUpload(ctx, actor, e.ID, ConfirmAttachmentInput{
		ObjectKey: otherTenant, FileName: "x.pdf", ContentType: "application/pdf", SizeBytes: 10,
	})
	assert.Equal(t, http.StatusBadRequest, httpStatus(err))
}

func TestAttachmentWithoutStoreReturns503(t *testing.T) {
	database := newDB(t)
	svc := NewAttachmentService(database, nil, zap.NewNop())
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)

	_, err := svc.PresignUpload(ctx, Actor{UserID: user, Role: tenantctx.RoleMember}, uuid.New(),
		PresignAttachmentInput{FileName: "a.png", ContentType: "image/png", SizeBytes: 1})
	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, httpStatus(err))
}
