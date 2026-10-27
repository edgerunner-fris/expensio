package notification

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

// Repository 只写 SQL，事务由 service / consumer 提供。
type Repository struct{}

func NewRepository() *Repository { return &Repository{} }

// List 返回当前用户的收件箱。user_id 由 JWT 提供，绝不来自查询参数。
func (r *Repository) List(ctx context.Context, tx pgx.Tx, userID uuid.UUID, unreadOnly bool, limit, offset int) ([]Notification, int64, error) {
	const countQ = `SELECT count(*) FROM app.notifications WHERE user_id = $1 AND ($2 = false OR read_at IS NULL)`
	var total int64
	if err := tx.QueryRow(ctx, countQ, userID, unreadOnly).Scan(&total); err != nil {
		return nil, 0, err
	}

	const q = `SELECT id, kind, title, body, COALESCE(resource_type, ''), resource_id, read_at, created_at
	           FROM app.notifications
	           WHERE user_id = $1 AND ($2 = false OR read_at IS NULL)
	           ORDER BY created_at DESC, id
	           LIMIT $3 OFFSET $4`
	rows, err := tx.Query(ctx, q, userID, unreadOnly, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]Notification, 0, limit)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.ResourceType,
			&n.ResourceID, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, 0, err
		}
		n.CreatedAt = n.CreatedAt.UTC()
		items = append(items, n)
	}
	return items, total, rows.Err()
}

func (r *Repository) UnreadCount(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	const q = `SELECT count(*) FROM app.notifications WHERE user_id = $1 AND read_at IS NULL`
	var count int64
	err := tx.QueryRow(ctx, q, userID).Scan(&count)
	return count, err
}

// MarkRead 幂等：已读的行只保留第一次的 read_at。
func (r *Repository) MarkRead(ctx context.Context, tx pgx.Tx, id, userID uuid.UUID) (bool, error) {
	const q = `UPDATE app.notifications
	           SET read_at = COALESCE(read_at, now())
	           WHERE id = $1 AND user_id = $2`
	tag, err := tx.Exec(ctx, q, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *Repository) MarkAllRead(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int64, error) {
	const q = `UPDATE app.notifications
	           SET read_at = now()
	           WHERE user_id = $1 AND read_at IS NULL`
	tag, err := tx.Exec(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// markProcessed 返回 true 表示这条事件第一次被这个消费者看到。
func (r *Repository) markProcessed(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, consumer string, eventID uuid.UUID) (bool, error) {
	const q = `INSERT INTO app.processed_events (tenant_id, consumer, event_id)
	           VALUES ($1, $2, $3)
	           ON CONFLICT (tenant_id, consumer, event_id) DO NOTHING`
	tag, err := tx.Exec(ctx, q, tenantID, consumer, eventID)
	if err != nil {
		return false, fmt.Errorf("记录幂等键失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Repository) insertNotification(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, d draft, eventID uuid.UUID) error {
	const q = `INSERT INTO app.notifications
	           (id, tenant_id, user_id, kind, title, body, resource_type, resource_id, event_id)
	           VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9)
	           ON CONFLICT (tenant_id, user_id, event_id, kind) DO NOTHING`
	_, err := tx.Exec(ctx, q, uuid.New(), tenantID, d.Recipient.UserID, d.Kind,
		d.Title, d.Body, d.Resource, d.ResourceID, eventID)
	return err
}

func (r *Repository) insertEmail(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, o Outbound) error {
	const q = `INSERT INTO app.email_outbox
	           (id, tenant_id, recipient, subject, body, dedupe_key)
	           VALUES ($1, $2, $3, $4, $5, $6)
	           ON CONFLICT (tenant_id, dedupe_key) DO NOTHING`
	_, err := tx.Exec(ctx, q, uuid.New(), tenantID, o.Recipient, o.Subject, o.Body, o.DedupeKey)
	return err
}

// listQuery 是收件箱查询参数。字段与 PageQuery 平铺而不是内嵌：
// 查询参数绑定走反射，平铺最不容易在升级 gin 时悄悄失效。
type listQuery struct {
	Page       int  `form:"page"        json:"page"`
	PageSize   int  `form:"page_size"   json:"page_size"`
	UnreadOnly bool `form:"unread_only" json:"unread_only"`
}

func (q listQuery) page() httpx.PageQuery {
	p := httpx.PageQuery{Page: q.Page, PageSize: q.PageSize}
	p.Normalize()
	return p
}
