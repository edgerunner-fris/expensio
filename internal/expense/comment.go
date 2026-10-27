package expense

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// maxCommentBodyRunes 与 expense_comments_body_check 一致。
const maxCommentBodyRunes = 2000

// CommentInput 是新增评论的请求体。
type CommentInput struct {
	Body string `json:"body"`
}

// commentView 是评论的对外形态。只暴露作者 ID，不附带邮箱或姓名：
// 前端已有成员列表，聚合用户资料是另一个服务的职责。
type commentView struct {
	ID        uuid.UUID `json:"id"`
	AuthorID  uuid.UUID `json:"author_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func newCommentView(c *Comment) commentView {
	return commentView{ID: c.ID, AuthorID: c.AuthorID, Body: c.Body, CreatedAt: c.CreatedAt}
}

// ListComments 返回一张单的评论。能查看单据即可查看评论。
func (s *Service) ListComments(ctx context.Context, actor Actor, expenseID uuid.UUID) ([]Comment, error) {
	var out []Comment
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if !CanView(actor, e) {
			return apperr.NotFound("费用单")
		}
		out, err = s.repo.ListComments(ctx, tx, e.TenantID, e.ID)
		return err
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// AddComment 新增评论。任何能查看该单的成员都可以留言，
// 包括已提交、已批准的单据：审批沟通不应被状态机挡住。
func (s *Service) AddComment(ctx context.Context, actor Actor, expenseID uuid.UUID, in CommentInput) (*Comment, error) {
	if !actor.valid() {
		return nil, apperr.Unauthorized("缺少有效的身份信息")
	}
	if !actor.Role.AtLeast(tenantctx.RoleMember) {
		return nil, apperr.Forbidden("只读成员不能评论")
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return nil, apperr.InvalidArgument("评论不能为空")
	}
	if utf8.RuneCountInString(body) > maxCommentBodyRunes {
		return nil, apperr.InvalidArgument("评论不超过 2000 个字符")
	}

	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}
	c := &Comment{
		ID:        uuid.New(),
		TenantID:  tenantID,
		ExpenseID: expenseID,
		AuthorID:  actor.UserID,
		Body:      body,
	}
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, expenseID)
		if err != nil {
			return err
		}
		if !CanView(actor, e) {
			return apperr.NotFound("费用单")
		}
		return s.repo.InsertComment(ctx, tx, c)
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return c, nil
}

// DeleteComment 软删评论。作者可以删自己的；管理岗可以删任何一条（内容治理）。
func (s *Service) DeleteComment(ctx context.Context, actor Actor, expenseID, commentID uuid.UUID) error {
	if !actor.valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		c, err := s.repo.GetComment(ctx, tx, expenseID, commentID)
		if err != nil {
			return err
		}
		if c.AuthorID != actor.UserID && !actor.manage() {
			return apperr.Forbidden("只能删除自己的评论")
		}
		return s.repo.SoftDeleteComment(ctx, tx, c.TenantID, expenseID, commentID)
	})
	return mapDBError(err)
}

func (r *Repository) ListComments(ctx context.Context, tx pgx.Tx, tenantID, expenseID uuid.UUID) ([]Comment, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, expense_id, author_id, body, created_at, deleted_at
		FROM app.expense_comments
		WHERE tenant_id = $1 AND expense_id = $2 AND deleted_at IS NULL
		ORDER BY created_at, id`, tenantID, expenseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Comment, 0, 8)
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExpenseID, &c.AuthorID,
			&c.Body, &c.CreatedAt, &c.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repository) GetComment(ctx context.Context, tx pgx.Tx, expenseID, commentID uuid.UUID) (*Comment, error) {
	var c Comment
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, expense_id, author_id, body, created_at, deleted_at
		FROM app.expense_comments
		WHERE expense_id = $1 AND id = $2 AND deleted_at IS NULL`,
		expenseID, commentID).
		Scan(&c.ID, &c.TenantID, &c.ExpenseID, &c.AuthorID, &c.Body, &c.CreatedAt, &c.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repository) InsertComment(ctx context.Context, tx pgx.Tx, c *Comment) error {
	return tx.QueryRow(ctx, `
		INSERT INTO app.expense_comments (id, tenant_id, expense_id, author_id, body)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		c.ID, c.TenantID, c.ExpenseID, c.AuthorID, c.Body).Scan(&c.CreatedAt)
}

func (r *Repository) SoftDeleteComment(ctx context.Context, tx pgx.Tx, tenantID, expenseID, commentID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE app.expense_comments SET deleted_at = now()
		WHERE tenant_id = $1 AND expense_id = $2 AND id = $3 AND deleted_at IS NULL`,
		tenantID, expenseID, commentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (h *Handler) ListComments(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	comments, err := h.svc.ListComments(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]commentView, 0, len(comments))
	for i := range comments {
		items = append(items, newCommentView(&comments[i]))
	}
	httpx.OK(c, gin.H{"items": items})
}

func (h *Handler) AddComment(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in CommentInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	comment, err := h.svc.AddComment(c.Request.Context(), actor, id, in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.Created(c, newCommentView(comment))
}

func (h *Handler) DeleteComment(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	commentID, parseErr := uuid.Parse(c.Param("commentID"))
	if parseErr != nil {
		httpx.Error(c, apperr.InvalidArgument("评论 ID 非法"))
		return
	}
	if err := h.svc.DeleteComment(c.Request.Context(), actor, id, commentID); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
