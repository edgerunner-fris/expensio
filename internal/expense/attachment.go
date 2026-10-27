package expense

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	// MaxAttachmentBytes 与 expense_attachments_size_check 一致：10 MiB。
	MaxAttachmentBytes int64 = 10 << 20
	// MaxAttachmentsPerExpense 防止单张单被当成无限图床。
	MaxAttachmentsPerExpense = 10
	// maxAttachmentNameRunes 与数据库 CHECK 一致。
	maxAttachmentNameRunes = 200

	attachmentUploadTTL   = 15 * time.Minute
	attachmentDownloadTTL = 10 * time.Minute
)

// attachmentTypes 是允许的内容类型与对象键扩展名的唯一映射。
// 扩展名不从用户文件名推断：文件名可以随便写，内容类型才是白名单的依据。
var attachmentTypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// objectKeyPattern 与数据库 CHECK 等价。捕获组用于校验租户与单据前缀。
var objectKeyPattern = regexp.MustCompile(
	`^tenants/([0-9a-f-]{36})/expenses/([0-9a-f-]{36})/([0-9a-f-]{36}\.(?:jpg|png|webp|pdf))$`)

// ObjectStore 是附件服务需要的对象存储能力。接口放在使用方，
// 存储实现（MinIO/S3）不反向依赖领域包，测试也可以换成真实容器。
type ObjectStore interface {
	PresignPut(ctx context.Context, key string, expiry time.Duration, contentType string) (string, error)
	PresignGet(ctx context.Context, key string, expiry time.Duration, fileName string) (string, error)
	Stat(ctx context.Context, key string) (size int64, contentType string, exists bool, err error)
	Remove(ctx context.Context, key string) error
}

// AttachmentService 负责附件元数据与对象存储的协调。
// 文件字节不经过本服务：上传与下载都由浏览器直连对象存储。
type AttachmentService struct {
	database *db.DB
	repo     *Repository
	store    ObjectStore
	log      *zap.Logger
	now      func() time.Time
}

func NewAttachmentService(database *db.DB, store ObjectStore, log *zap.Logger) *AttachmentService {
	if log == nil {
		log = zap.NewNop()
	}
	return &AttachmentService{
		database: database,
		repo:     NewRepository(),
		store:    store,
		log:      log,
		now:      time.Now,
	}
}

// PresignAttachmentInput 是申请上传 URL 的请求体。
type PresignAttachmentInput struct {
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// ConfirmAttachmentInput 是上传完成后确认的请求体。
// ObjectKey 必须与 presign 返回的一致。
type ConfirmAttachmentInput struct {
	ObjectKey   string `json:"object_key"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// PresignUpload 是返回给浏览器的上传指令。
type PresignUpload struct {
	ObjectKey string            `json:"object_key"`
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// AttachmentView 是附件列表项，下载 URL 每次现签。
type AttachmentView struct {
	ID                uuid.UUID `json:"id"`
	FileName          string    `json:"file_name"`
	ContentType       string    `json:"content_type"`
	SizeBytes         int64     `json:"size_bytes"`
	UploadedBy        uuid.UUID `json:"uploaded_by"`
	CreatedAt         time.Time `json:"created_at"`
	DownloadURL       string    `json:"download_url"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
}

// validateAttachmentMeta 归一化并校验文件名、类型与大小。
func validateAttachmentMeta(fileName, contentType string, size int64) (string, string, string, error) {
	name := strings.TrimSpace(fileName)
	if name == "" {
		return "", "", "", apperr.InvalidArgument("文件名不能为空")
	}
	if utf8.RuneCountInString(name) > maxAttachmentNameRunes {
		return "", "", "", apperr.InvalidArgument("文件名不超过 200 个字符")
	}
	// 文件名只用于展示与下载头；出现路径分隔符通常意味着客户端在试探路径。
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", "", "", apperr.InvalidArgument("文件名不能包含路径分隔符")
	}
	ext, ok := attachmentTypes[contentType]
	if !ok {
		return "", "", "", apperr.InvalidArgument("只允许 JPEG、PNG、WebP 或 PDF 附件")
	}
	if size < 1 || size > MaxAttachmentBytes {
		return "", "", "", apperr.InvalidArgument("附件大小必须在 1 字节到 10 MiB 之间")
	}
	return name, contentType, ext, nil
}

func buildObjectKey(tenantID, expenseID uuid.UUID, ext string) string {
	return "tenants/" + tenantID.String() + "/expenses/" + expenseID.String() +
		"/" + uuid.NewString() + ext
}

// validateObjectKey 校验对象键确实属于这张单。没有这一步，同租户的另一张单
// 的对象键也能被挂过来，等于绕过"附件属于哪张报销单"的边界。
func validateObjectKey(key string, tenantID, expenseID uuid.UUID) error {
	m := objectKeyPattern.FindStringSubmatch(key)
	if m == nil || m[1] != tenantID.String() || m[2] != expenseID.String() {
		return apperr.InvalidArgument("对象键不属于当前费用单").WithCause(
			apperr.New(CodeAttachmentObject, 400, "invalid object key"))
	}
	return nil
}

// authorizeAttachmentWrite 只允许草稿的提交人增删附件。
// 提交后附件是审批证据，必须冻结。
func authorizeAttachmentWrite(actor Actor, e *Expense) error {
	if !actor.valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	if e.Deleted() {
		return apperr.NotFound("费用单")
	}
	if !actor.Role.AtLeast(tenantctx.RoleMember) {
		return apperr.Forbidden("只读成员不能管理附件")
	}
	if e.SubmitterID != actor.UserID {
		return apperr.Forbidden("只有提交人可以管理附件")
	}
	if !e.IsDraft() {
		return apperr.Conflict(CodeStateConflict, "费用单提交后附件不可修改").
			WithDetail("status", string(e.Status))
	}
	return nil
}

func (s *AttachmentService) storageReady() error {
	if s.store == nil {
		return apperr.New(CodeAttachmentStorage, 503, "附件存储未配置")
	}
	return nil
}

// PresignUpload 校验权限与元数据后签发直传 URL。
// 数量检查在这里只是提前失败；确认接口会在行锁内再查一次。
func (s *AttachmentService) PresignUpload(ctx context.Context, actor Actor, expenseID uuid.UUID, in PresignAttachmentInput) (*PresignUpload, error) {
	if err := s.storageReady(); err != nil {
		return nil, err
	}
	_, contentType, ext, err := validateAttachmentMeta(in.FileName, in.ContentType, in.SizeBytes)
	if err != nil {
		return nil, err
	}

	var key string
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if err := authorizeAttachmentWrite(actor, e); err != nil {
			return err
		}
		count, err := s.repo.CountAttachments(ctx, tx, e.TenantID, e.ID)
		if err != nil {
			return err
		}
		if count >= MaxAttachmentsPerExpense {
			return apperr.Conflict(CodeAttachmentLimit, "每张费用单最多 10 个附件")
		}
		key = buildObjectKey(e.TenantID, e.ID, ext)
		return nil
	})
	if err != nil {
		return nil, mapDBError(err)
	}

	uploadURL, err := s.store.PresignPut(ctx, key, attachmentUploadTTL, contentType)
	if err != nil {
		s.log.Error("presign upload failed", zap.Error(err))
		return nil, apperr.New(CodeAttachmentStorage, 503, "附件存储暂不可用，请稍后重试").WithCause(err)
	}
	return &PresignUpload{
		ObjectKey: key,
		UploadURL: uploadURL,
		Method:    "PUT",
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: s.now().Add(attachmentUploadTTL),
	}, nil
}

// ConfirmUpload 在对象确实存在且大小一致后才写入元数据。
// Stat 放在事务里是刻意的：确认是草稿阶段低频操作，用它换"检查与插入"
// 之间没有并发窗口，比多一次事务更简单。
func (s *AttachmentService) ConfirmUpload(ctx context.Context, actor Actor, expenseID uuid.UUID, in ConfirmAttachmentInput) (*Attachment, error) {
	if err := s.storageReady(); err != nil {
		return nil, err
	}
	name, contentType, _, err := validateAttachmentMeta(in.FileName, in.ContentType, in.SizeBytes)
	if err != nil {
		return nil, err
	}

	var out *Attachment
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.GetForUpdate(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if err := authorizeAttachmentWrite(actor, e); err != nil {
			return err
		}
		if err := validateObjectKey(in.ObjectKey, e.TenantID, e.ID); err != nil {
			return err
		}
		count, err := s.repo.CountAttachments(ctx, tx, e.TenantID, e.ID)
		if err != nil {
			return err
		}
		if count >= MaxAttachmentsPerExpense {
			return apperr.Conflict(CodeAttachmentLimit, "每张费用单最多 10 个附件")
		}

		size, storedType, exists, err := s.store.Stat(ctx, in.ObjectKey)
		if err != nil {
			s.log.Error("stat attachment object failed", zap.Error(err))
			return apperr.New(CodeAttachmentStorage, 503, "附件存储暂不可用，请稍后重试").WithCause(err)
		}
		if !exists {
			return apperr.InvalidArgument("对象不存在，请先完成上传")
		}
		if size != in.SizeBytes {
			return apperr.InvalidArgument("对象大小与声明不一致")
		}
		if storedType != "" && storedType != contentType {
			return apperr.InvalidArgument("对象内容类型与声明不一致")
		}

		a := &Attachment{
			ID:          uuid.New(),
			TenantID:    e.TenantID,
			ExpenseID:   e.ID,
			ObjectKey:   in.ObjectKey,
			FileName:    name,
			ContentType: contentType,
			SizeBytes:   size,
			UploadedBy:  actor.UserID,
		}
		if err := s.repo.InsertAttachment(ctx, tx, a); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return apperr.Conflict(CodeAttachmentObject, "该对象已关联到费用单")
			}
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// List 返回附件元数据与短期下载 URL。URL 不落库，每次现签。
func (s *AttachmentService) List(ctx context.Context, actor Actor, expenseID uuid.UUID) ([]AttachmentView, error) {
	if err := s.storageReady(); err != nil {
		return nil, err
	}
	var items []Attachment
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if !CanView(actor, e) {
			return apperr.NotFound("费用单")
		}
		items, err = s.repo.ListAttachments(ctx, tx, e.TenantID, e.ID)
		return err
	})
	if err != nil {
		return nil, mapDBError(err)
	}

	expiresAt := s.now().Add(attachmentDownloadTTL)
	out := make([]AttachmentView, 0, len(items))
	for i := range items {
		url, err := s.store.PresignGet(ctx, items[i].ObjectKey, attachmentDownloadTTL, items[i].FileName)
		if err != nil {
			s.log.Error("presign download failed", zap.Error(err))
			return nil, apperr.New(CodeAttachmentStorage, 503, "附件存储暂不可用，请稍后重试").WithCause(err)
		}
		out = append(out, AttachmentView{
			ID:                items[i].ID,
			FileName:          items[i].FileName,
			ContentType:       items[i].ContentType,
			SizeBytes:         items[i].SizeBytes,
			UploadedBy:        items[i].UploadedBy,
			CreatedAt:         items[i].CreatedAt,
			DownloadURL:       url,
			DownloadExpiresAt: expiresAt,
		})
	}
	return out, nil
}

// Delete 软删元数据，再尽力删除对象。对象删除失败只记日志：
// 元数据已经不可见，残留对象交给桶的生命周期规则清理。
func (s *AttachmentService) Delete(ctx context.Context, actor Actor, expenseID, attachmentID uuid.UUID) error {
	if err := s.storageReady(); err != nil {
		return err
	}
	var key string
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.GetForUpdate(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if err := authorizeAttachmentWrite(actor, e); err != nil {
			return err
		}
		a, err := s.repo.GetAttachment(ctx, tx, e.TenantID, e.ID, attachmentID)
		if err != nil {
			return err
		}
		key = a.ObjectKey
		return s.repo.SoftDeleteAttachment(ctx, tx, e.TenantID, e.ID, attachmentID)
	})
	if err != nil {
		return mapDBError(err)
	}
	if err := s.store.Remove(ctx, key); err != nil {
		s.log.Warn("remove attachment object failed", zap.String("object_key", key), zap.Error(err))
	}
	return nil
}

func (r *Repository) CountAttachments(ctx context.Context, tx pgx.Tx, tenantID, expenseID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM app.expense_attachments
		WHERE tenant_id = $1 AND expense_id = $2 AND deleted_at IS NULL`,
		tenantID, expenseID).Scan(&n)
	return n, err
}

func (r *Repository) ListAttachments(ctx context.Context, tx pgx.Tx, tenantID, expenseID uuid.UUID) ([]Attachment, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, expense_id, object_key, file_name, content_type,
		       size_bytes, uploaded_by, created_at, deleted_at
		FROM app.expense_attachments
		WHERE tenant_id = $1 AND expense_id = $2 AND deleted_at IS NULL
		ORDER BY created_at, id`, tenantID, expenseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Attachment, 0, 4)
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.TenantID, &a.ExpenseID, &a.ObjectKey, &a.FileName,
			&a.ContentType, &a.SizeBytes, &a.UploadedBy, &a.CreatedAt, &a.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) GetAttachment(ctx context.Context, tx pgx.Tx, tenantID, expenseID, attachmentID uuid.UUID) (*Attachment, error) {
	var a Attachment
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, expense_id, object_key, file_name, content_type,
		       size_bytes, uploaded_by, created_at, deleted_at
		FROM app.expense_attachments
		WHERE tenant_id = $1 AND expense_id = $2 AND id = $3 AND deleted_at IS NULL`,
		tenantID, expenseID, attachmentID).
		Scan(&a.ID, &a.TenantID, &a.ExpenseID, &a.ObjectKey, &a.FileName,
			&a.ContentType, &a.SizeBytes, &a.UploadedBy, &a.CreatedAt, &a.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *Repository) InsertAttachment(ctx context.Context, tx pgx.Tx, a *Attachment) error {
	return tx.QueryRow(ctx, `
		INSERT INTO app.expense_attachments
		    (id, tenant_id, expense_id, object_key, file_name, content_type,
		     size_bytes, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at`,
		a.ID, a.TenantID, a.ExpenseID, a.ObjectKey, a.FileName,
		a.ContentType, a.SizeBytes, a.UploadedBy).Scan(&a.CreatedAt)
}

func (r *Repository) SoftDeleteAttachment(ctx context.Context, tx pgx.Tx, tenantID, expenseID, attachmentID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE app.expense_attachments SET deleted_at = now()
		WHERE tenant_id = $1 AND expense_id = $2 AND id = $3 AND deleted_at IS NULL`,
		tenantID, expenseID, attachmentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
