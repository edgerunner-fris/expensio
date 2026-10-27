package notification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
)

// emailMaxAttempts 必须与 app.claim_emails 里的 attempts < 5 一致：
// 领取端不再发放超过上限的邮件，worker 负责把失败原因写回。
const emailMaxAttempts = 5

const emailTelemetryName = "github.com/edgerunner-fris/expensio/internal/notification"

var emailResults = telemetry.NewCounter(
	"expensio.notification.emails",
	"邮件投递结果（sent/failed）；final=true 表示已达到最大重试次数",
	"{email}",
)

// EmailWorker 周期领取 email_outbox 里的待发邮件并投递。
//
// 投递语义是至少一次：邮件发出后、状态写回前进程崩溃，5 分钟后会被重新领取，
// 收件人可能收到重复邮件。对内部通知来说，重复远好于丢失。
type EmailWorker struct {
	database *db.DB
	mailer   Mailer
	logger   *zap.Logger
	interval time.Duration
	batch    int
	now      func() time.Time
}

type EmailWorkerOptions struct {
	Database *db.DB
	Mailer   Mailer
	Logger   *zap.Logger
	Interval time.Duration
	Batch    int
}

func NewEmailWorker(opts EmailWorkerOptions) (*EmailWorker, error) {
	if opts.Database == nil {
		return nil, errors.New("notification: worker 需要 database")
	}
	if opts.Mailer == nil {
		return nil, errors.New("notification: worker 需要 mailer")
	}
	if opts.Logger == nil {
		return nil, errors.New("notification: worker 需要 logger")
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.Batch <= 0 || opts.Batch > 100 {
		opts.Batch = 20
	}
	return &EmailWorker{
		database: opts.Database,
		mailer:   opts.Mailer,
		logger:   opts.Logger,
		interval: opts.Interval,
		batch:    opts.Batch,
		now:      time.Now,
	}, nil
}

// Run 阻塞到 ctx 取消。单次失败只记录，不会终止 worker：
// 邮件是尽力而为的派生动作，不应该拖垮整个服务。
func (w *EmailWorker) Run(ctx context.Context) {
	w.runOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *EmailWorker) runOnce(ctx context.Context) {
	if _, err := w.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
		w.logger.Warn("email worker iteration failed", zap.Error(err))
	}
}

type claimedEmail struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Recipient string
	Subject   string
	Body      string
	Attempts  int
}

// ProcessOnce 领取一批邮件并逐封投递，返回处理数量。导出便于集成测试直接驱动。
func (w *EmailWorker) ProcessOnce(ctx context.Context) (int, error) {
	claimed, err := w.claim(ctx)
	if err != nil {
		return 0, err
	}
	for _, mail := range claimed {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		w.deliver(ctx, mail)
	}
	return len(claimed), nil
}

func (w *EmailWorker) claim(ctx context.Context) ([]claimedEmail, error) {
	var claimed []claimedEmail
	err := w.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, recipient, subject, body, attempts
			 FROM app.claim_emails($1)`, w.batch)
		if err != nil {
			return fmt.Errorf("notification: 领取邮件失败: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m claimedEmail
			if err := rows.Scan(&m.ID, &m.TenantID, &m.Recipient, &m.Subject, &m.Body, &m.Attempts); err != nil {
				return err
			}
			claimed = append(claimed, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (w *EmailWorker) deliver(ctx context.Context, mail claimedEmail) {
	// 只记录邮件 ID 与第几次尝试，绝不把收件地址或正文放进 span。
	ctx, span := otel.Tracer(emailTelemetryName).Start(ctx, "notification.email.deliver",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("email.id", mail.ID.String()),
			attribute.Int("email.attempt", mail.Attempts),
		))
	defer span.End()

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := w.mailer.Send(sendCtx, mail.Recipient, mail.Subject, mail.Body)
	cancel()

	if err == nil {
		emailResults.Add(ctx, 1, attribute.String("outcome", "sent"))
		if markErr := w.markSent(ctx, mail.ID, mail.TenantID); markErr != nil {
			// 状态写回失败会让这封邮件在 5 分钟后被重新领取：
			// 宁可重复，也不能把"可能已发出"当成已发。
			w.logger.Warn("email sent but status update failed",
				zap.String("email_id", mail.ID.String()), zap.Error(markErr))
			return
		}
		w.logger.Info("email sent", zap.String("email_id", mail.ID.String()))
		return
	}

	if ctx.Err() != nil {
		// 进程正在退出：保持 sending，交给下一次启动的回收逻辑。
		return
	}

	emailResults.Add(ctx, 1,
		attribute.String("outcome", "failed"),
		attribute.Bool("final", mail.Attempts >= emailMaxAttempts))
	span.RecordError(err)
	span.SetStatus(codes.Error, "email delivery failed")

	if markErr := w.markFailed(ctx, mail, err); markErr != nil {
		w.logger.Warn("email failed and status update failed",
			zap.String("email_id", mail.ID.String()), zap.Error(markErr))
		return
	}
	w.logger.Warn("email delivery failed",
		zap.String("email_id", mail.ID.String()),
		zap.Int("attempts", mail.Attempts),
		zap.Error(err))
}

func (w *EmailWorker) markSent(ctx context.Context, id, tenantID uuid.UUID) error {
	return w.database.WithinTenantID(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE app.email_outbox
			SET status = 'sent', sent_at = now(), last_error = NULL
			WHERE id = $1`, id)
		return err
	})
}

func (w *EmailWorker) markFailed(ctx context.Context, mail claimedEmail, sendErr error) error {
	status := "failed"
	nextAttempt := w.now().Add(emailBackoff(mail.Attempts))
	if mail.Attempts >= emailMaxAttempts {
		// 已达上限：保留 failed 状态供人工排查；claim_emails 不会再领取。
		nextAttempt = w.now().Add(365 * 24 * time.Hour)
	}

	return w.database.WithinTenantID(ctx, mail.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE app.email_outbox
			SET status = $2, last_error = left($3, 1000), next_attempt_at = $4
			WHERE id = $1`, mail.ID, status, sendErr.Error(), nextAttempt)
		return err
	})
}

// emailBackoff 以领取次数为指数退避，最长 1 小时。
func emailBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 6 {
		return time.Hour
	}
	d := time.Minute << (attempts - 1)
	if d > time.Hour {
		return time.Hour
	}
	return d
}
