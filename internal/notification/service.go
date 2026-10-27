package notification

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Service 提供收件箱的读写。所有方法都要求 ctx 里已经有 JWT 派生的租户上下文，
// user_id 同样只来自该上下文——接口参数里没有"查谁的通知"这种字段。
type Service struct {
	database *db.DB
	repo     *Repository
}

func NewService(database *db.DB) *Service {
	return &Service{database: database, repo: NewRepository()}
}

func actorFrom(ctx context.Context) (tenantctx.Tenant, error) {
	actor, ok := tenantctx.From(ctx)
	if !ok {
		return tenantctx.Tenant{}, apperr.Unauthorized("缺少访问令牌")
	}
	return actor, nil
}

func (s *Service) List(ctx context.Context, q listQuery) (httpx.Page[Notification], error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return httpx.Page[Notification]{}, err
	}
	page := q.page()

	var items []Notification
	var total int64
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var listErr error
		items, total, listErr = s.repo.List(ctx, tx, actor.UserID, q.UnreadOnly, page.PageSize, page.Offset())
		return listErr
	})
	if err != nil {
		return httpx.Page[Notification]{}, wrapDB(err, "读取通知失败")
	}
	return httpx.NewPage(items, total, page), nil
}

func (s *Service) UnreadCount(ctx context.Context) (int64, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var countErr error
		count, countErr = s.repo.UnreadCount(ctx, tx, actor.UserID)
		return countErr
	})
	if err != nil {
		return 0, wrapDB(err, "读取未读数量失败")
	}
	return count, nil
}

func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	actor, err := actorFrom(ctx)
	if err != nil {
		return err
	}
	var found bool
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var markErr error
		found, markErr = s.repo.MarkRead(ctx, tx, id, actor.UserID)
		return markErr
	})
	if err != nil {
		return wrapDB(err, "标记已读失败")
	}
	if !found {
		return apperr.NotFound("通知")
	}
	return nil
}

func (s *Service) MarkAllRead(ctx context.Context) (int64, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return 0, err
	}
	var affected int64
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var markErr error
		affected, markErr = s.repo.MarkAllRead(ctx, tx, actor.UserID)
		return markErr
	})
	if err != nil {
		return 0, wrapDB(err, "全部标记已读失败")
	}
	return affected, nil
}

// EnqueueEmail 供受信任的内部服务写入发件箱。
//
// tenantID 来自 serviceauth 签名载荷，不是请求体；调用方在 handler 里
// 已经验证过签名。字段校验放在这里，确保服务间接口与事件消费路径
// 对"一封合法邮件"的定义一致。
func (s *Service) EnqueueEmail(ctx context.Context, tenantID uuid.UUID, o Outbound) error {
	if tenantID == uuid.Nil {
		return apperr.InvalidArgument("租户 ID 非法")
	}
	if err := validateOutbound(o); err != nil {
		return err
	}
	err := s.database.WithinTenantID(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.insertEmail(ctx, tx, tenantID, o)
	})
	if err != nil {
		return wrapDB(err, "写入邮件发件箱失败")
	}
	return nil
}

func validateOutbound(o Outbound) error {
	recipient := strings.TrimSpace(o.Recipient)
	at := strings.IndexByte(recipient, '@')
	if len(recipient) < 3 || len(recipient) > 320 || at <= 0 || at == len(recipient)-1 ||
		strings.Count(recipient, "@") != 1 || strings.ContainsAny(recipient, " \t\r\n") {
		return apperr.InvalidArgument("收件人邮箱非法")
	}
	subject := strings.TrimSpace(o.Subject)
	if subject == "" || utf8.RuneCountInString(subject) > 200 {
		return apperr.InvalidArgument("邮件主题不能为空且不超过 200 个字符")
	}
	if utf8.RuneCountInString(o.Body) > 10000 {
		return apperr.InvalidArgument("邮件正文不超过 10000 个字符")
	}
	key := strings.TrimSpace(o.DedupeKey)
	if key == "" || len(key) > 200 {
		return apperr.InvalidArgument("幂等键不能为空且不超过 200 个字节")
	}
	return nil
}

// wrapDB 把驱动层错误变成 500，不把 SQL 细节带进响应。
// 已经带有业务语义的错误（例如 NotFound）保持原样。
func wrapDB(err error, msg string) error {
	if _, ok := err.(*apperr.Error); ok {
		return err
	}
	return apperr.Internal(fmt.Errorf("%s: %w", msg, err))
}
