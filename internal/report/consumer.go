// report 消费者：把费用事件写入读模型。
//
// 幂等分两层：
//   - processed_events 以 (tenant_id, consumer, event_id) 为主键，
//     同一事件重复投递只记一次；
//   - expense_facts 以 (tenant_id, expense_id) 为主键，并只在
//     excluded.version >= 当前 version 时覆盖，保证乱序到达的旧事件
//     不会把较新的状态回滚。
//
// 两层写入在同一个事务里，事务提交后消费者才提交 Kafka 位点，
// 因此"数据库里没有、位点却已提交"不可能发生；反过来（已写库、位点未提交）
// 只会导致重复投递，由上面的两层幂等吸收。
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
)

// ConsumerOptions 是报表消费者的外部依赖。
type ConsumerOptions struct {
	Database  *db.DB
	Brokers   []string
	Publisher kafka.Publisher
	Logger    *zap.Logger
	ClientID  string

	// Cache 可选：设置后每条事件提交成功都会推进租户缓存版本。
	Cache *DashboardCache

	// 以下字段只用于测试隔离；生产留空即采用 events 包里的契约常量。
	Topic        string
	GroupID      string
	DLQTopic     string
	ConsumerName string

	RetryBase time.Duration
	// Security 是 Kafka 连接的安全参数；零值表示本地明文、无认证。
	Security kafka.Security
}

// Consumer 实现 kafka.Handler。
type Consumer struct {
	database     *db.DB
	logger       *zap.Logger
	consumerName string
	consumer     *kafka.Consumer
	cache        *DashboardCache
}

// NewConsumer 装配消费端。
func NewConsumer(opts ConsumerOptions) (*Consumer, error) {
	if opts.Database == nil {
		return nil, errors.New("report: database 不能为空")
	}
	if opts.Logger == nil {
		return nil, errors.New("report: logger 不能为空")
	}
	if opts.Publisher == nil {
		return nil, errors.New("report: publisher 不能为空（DLQ 需要）")
	}

	topic := opts.Topic
	if topic == "" {
		topic = events.TopicExpense
	}
	groupID := opts.GroupID
	if groupID == "" {
		groupID = events.ConsumerReportExpense
	}
	dlqTopic := opts.DLQTopic
	if dlqTopic == "" {
		dlqTopic = events.TopicExpenseDLQ
	}
	consumerName := opts.ConsumerName
	if consumerName == "" {
		consumerName = events.ConsumerReportExpense
	}

	c := &Consumer{
		database:     opts.Database,
		logger:       opts.Logger,
		consumerName: consumerName,
		cache:        opts.Cache,
	}

	kc, err := kafka.NewConsumer(kafka.ConsumerOptions{
		Brokers:   opts.Brokers,
		GroupID:   groupID,
		Topic:     topic,
		DLQTopic:  dlqTopic,
		ClientID:  opts.ClientID,
		Handler:   c,
		Publisher: opts.Publisher,
		Logger:    opts.Logger,
		RetryBase: opts.RetryBase,
		Security:  opts.Security,
	})
	if err != nil {
		return nil, err
	}
	c.consumer = kc
	return c, nil
}

// Run 阻塞消费直到 ctx 取消。
func (c *Consumer) Run(ctx context.Context) error { return c.consumer.Run(ctx) }

// Close 释放 Kafka 客户端。应在 Run 返回之后调用。
func (c *Consumer) Close() { c.consumer.Close() }

// eventStatus 断言事件类型与快照状态一致。
// 两者不一致说明生产者有 bug 或消息被篡改，重试没有意义。
var eventStatus = map[events.Type]string{
	events.TypeExpenseSubmitted:  "submitted",
	events.TypeExpenseApproved:   "approved",
	events.TypeExpenseRejected:   "rejected",
	events.TypeExpenseCancelled:  "cancelled",
	events.TypeExpenseReimbursed: "reimbursed",
}

// Handle 处理一条事件。返回的错误已按永久/瞬时分类：
// 解码失败与契约冲突标为 Permanent，数据库瞬时故障保持原样以触发重试。
func (c *Consumer) Handle(ctx context.Context, msg kafka.Message) error {
	env, snap, err := decodeExpenseEvent(msg.Value)
	if err != nil {
		return kafka.Permanent(err)
	}

	var duplicate bool
	err = c.database.WithinTenantID(ctx, env.TenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				INSERT INTO app.processed_events (tenant_id, consumer, event_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (tenant_id, consumer, event_id) DO NOTHING`,
				env.TenantID, c.consumerName, env.EventID)
			if err != nil {
				return fmt.Errorf("记录幂等键失败: %w", err)
			}
			duplicate = tag.RowsAffected() == 0

			// 即使是重复事件也执行 upsert：它是幂等的，而且能修复
			// "幂等键已写入、事实行却缺失"这种只可能来自人为修数的状态。
			_, err = tx.Exec(ctx, `
				INSERT INTO app.expense_facts (
				    tenant_id, expense_id, submitter_id, status, category,
				    currency, amount_minor, occurred_at, budget_id, decided_by,
				    decided_at, version, expense_created_at, expense_updated_at,
				    last_event_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
				ON CONFLICT (tenant_id, expense_id) DO UPDATE SET
				    submitter_id       = excluded.submitter_id,
				    status             = excluded.status,
				    category           = excluded.category,
				    currency           = excluded.currency,
				    amount_minor       = excluded.amount_minor,
				    occurred_at        = excluded.occurred_at,
				    budget_id          = excluded.budget_id,
				    decided_by         = excluded.decided_by,
				    decided_at         = excluded.decided_at,
				    version            = excluded.version,
				    expense_created_at = excluded.expense_created_at,
				    expense_updated_at = excluded.expense_updated_at,
				    last_event_id      = excluded.last_event_id
				WHERE excluded.version >= app.expense_facts.version`,
				env.TenantID, snap.ID, snap.SubmitterID, snap.Status, snap.Category,
				snap.Currency, snap.AmountMinor, snap.OccurredAt, snap.BudgetID,
				snap.DecidedBy, snap.DecidedAt, snap.Version, snap.CreatedAt,
				snap.UpdatedAt, env.EventID)
			if err != nil {
				return fmt.Errorf("写入事实失败: %w", err)
			}
			return nil
		})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return classifyDBError(err)
	}

	// 事务已提交：推进缓存版本。失败只影响缓存新鲜度，不回滚、不阻塞位点提交。
	c.cache.Bump(ctx, env.TenantID)

	if duplicate {
		c.logger.Debug("duplicate event ignored",
			zap.String("event_id", env.EventID.String()),
			zap.String("expense_id", snap.ID.String()))
	}
	return nil
}

// decodeExpenseEvent 解码并校验事件，任何契约冲突都返回可用于 DLQ 的错误。
func decodeExpenseEvent(raw []byte) (events.Envelope, events.ExpenseSnapshot, error) {
	var snap events.ExpenseSnapshot

	env, err := events.Decode(raw)
	if err != nil {
		return events.Envelope{}, snap, err
	}
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		return events.Envelope{}, snap, fmt.Errorf("解码快照失败: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return events.Envelope{}, snap, fmt.Errorf("快照非法: %w", err)
	}
	if snap.TenantID != env.TenantID {
		return events.Envelope{}, snap, fmt.Errorf(
			"快照租户 %s 与信封租户 %s 不一致", snap.TenantID, env.TenantID)
	}
	if snap.ID != env.AggregateID {
		return events.Envelope{}, snap, fmt.Errorf(
			"快照费用 %s 与聚合 %s 不一致", snap.ID, env.AggregateID)
	}
	if snap.Version != env.AggregateVersion {
		return events.Envelope{}, snap, fmt.Errorf(
			"快照版本 %d 与信封版本 %d 不一致", snap.Version, env.AggregateVersion)
	}
	want, ok := eventStatus[env.Type]
	if !ok {
		return events.Envelope{}, snap, fmt.Errorf("事件类型 %q 没有对应的报表状态", env.Type)
	}
	if snap.Status != want {
		return events.Envelope{}, snap, fmt.Errorf(
			"事件类型 %s 要求状态 %s，快照却是 %s", env.Type, want, snap.Status)
	}
	return env, snap, nil
}

// classifyDBError 区分"重试能好"与"重试也不会好"。
//
// 数据异常（22）、约束冲突（23）、语法/权限（42）都源自消息内容或表结构，
// 同一条消息再试一万次结果一样；连接类错误则交给上层退避重试。
func classifyDBError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		switch pgErr.Code[:2] {
		case "22", "23", "42":
			return kafka.Permanent(err)
		}
	}
	return err
}
