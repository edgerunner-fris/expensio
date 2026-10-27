package expense

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

// attachmentService 返回附件服务；未配置对象存储时返回 503，而不是空指针解引用。
// 部署上附件是可选依赖：没有设置 MinIO 环境变量的环境仍要能跑完其余功能。
func (h *Handler) attachmentService() (*AttachmentService, error) {
	if h.attachments == nil {
		return nil, apperr.New(CodeAttachmentStorage, 503, "附件存储未配置")
	}
	return h.attachments, nil
}

func attachmentRequest(c *gin.Context) (Actor, uuid.UUID, error) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		return Actor{}, uuid.Nil, err
	}
	id, err := expenseID(c)
	if err != nil {
		return Actor{}, uuid.Nil, err
	}
	return actor, id, nil
}

func (h *Handler) PresignAttachment(c *gin.Context) {
	svc, err := h.attachmentService()
	if err != nil {
		httpx.Error(c, err)
		return
	}
	actor, id, err := attachmentRequest(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in PresignAttachmentInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	out, err := svc.PresignUpload(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, out)
}

func (h *Handler) ConfirmAttachment(c *gin.Context) {
	svc, err := h.attachmentService()
	if err != nil {
		httpx.Error(c, err)
		return
	}
	actor, id, err := attachmentRequest(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in ConfirmAttachmentInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	a, err := svc.ConfirmUpload(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, gin.H{
		"id":           a.ID,
		"file_name":    a.FileName,
		"content_type": a.ContentType,
		"size_bytes":   a.SizeBytes,
		"uploaded_by":  a.UploadedBy,
		"created_at":   a.CreatedAt,
	})
}

func (h *Handler) ListAttachments(c *gin.Context) {
	svc, err := h.attachmentService()
	if err != nil {
		httpx.Error(c, err)
		return
	}
	actor, id, err := attachmentRequest(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items, err := svc.List(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": items})
}

func (h *Handler) DeleteAttachment(c *gin.Context) {
	svc, err := h.attachmentService()
	if err != nil {
		httpx.Error(c, err)
		return
	}
	actor, id, err := attachmentRequest(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	attachmentID, parseErr := uuid.Parse(c.Param("attachmentID"))
	if parseErr != nil {
		httpx.Error(c, apperr.InvalidArgument("附件 ID 非法"))
		return
	}
	if err := svc.Delete(c.Request.Context(), actor, id, attachmentID); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
