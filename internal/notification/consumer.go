// 事件消费：把费用/账单事件转换成收件人可见的通知与待发邮件。
//
// 与 report 消费者相同的可靠性约定：
//   - 先取收件人（HTTP），再开事务。外部调用不占用数据库连接，也不在事务里等待网络。
//   - processed_events 写入与通知/发件箱写入在同一个事务里；
//     事务提交后 kafka 才提交位点。
//   - 永久错误（契约不符）进 DLQ，瞬时错误（identity 不可用、数据库断连）重试。
package notification

import (
	"context"
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

type topicKind int

const (
	topicExpense topicKind = iota + 1
	topicBilling
)

// ConsumerOptions 是两类消费者共用的装配参数。
type ConsumerOptions struct {
	Database     *db.DB
	Contacts     ContactsSource
	Brokers      []string
	Publisher    kafka.Publisher
	Logger       *zap.Logger
	ClientID     string
	EmailEnabled bool

	// 以下字段只用于测试隔离；生产留空即采用 events 包里的契约常量。
	Topic        string
	GroupID      string
	DLQTopic     string
	ConsumerName string

	RetryBase time.Duration

	// Security 是 Kafka 连接的安全参数；零值表示本地明文、无认证。
	Security kafka.Security
}

// Consumer 实现 kafka.Handler。一个实例只消费一个主题。
type Consumer struct {
	database     *db.DB
	contacts     ContactsSource
	repo         *Repository
	logger       *zap.Logger
	consumerName string
	emailEnabled bool
	kind         topicKind
	consumer     *kafka.Consumer
}

func NewExpenseConsumer(opts ConsumerOptions) (*Consumer, error) {
	return newConsumer(opts, topicExpense)
}

func NewBillingConsumer(opts ConsumerOptions) (*Consumer, error) {
	return newConsumer(opts, topicBilling)
}

func newConsumer(opts ConsumerOptions, kind topicKind) (*Consumer, error) {
	if opts.Database == nil {
		return nil, errors.New("notification: database 不能为空")
	}
	if opts.Contacts == nil {
		return nil, errors.New("notification: contacts 不能为空")
	}
	if opts.Logger == nil {
		return nil, errors.New("notification: logger 不能为空")
	}
	if opts.Publisher == nil {
		return nil, errors.New("notification: publisher 不能为空（DLQ 需要）")
	}

	topic, groupID, dlqTopic, name := opts.Topic, opts.GroupID, opts.DLQTopic, opts.ConsumerName
	switch kind {
	case topicExpense:
		if topic == "" {
			topic = events.TopicExpense
		}
		if groupID == "" {
			groupID = events.ConsumerNotificationExpense
		}
		if dlqTopic == "" {
			dlqTopic = events.TopicExpenseDLQ
		}
		if name == "" {
			name = events.ConsumerNotificationExpense
		}
	case topicBilling:
		if topic == "" {
			topic = events.TopicBilling
		}
		if groupID == "" {
			groupID = events.ConsumerNotificationBilling
		}
		if dlqTopic == "" {
			dlqTopic = events.TopicBillingDLQ
		}
		if name == "" {
			name = events.ConsumerNotificationBilling
		}
	default:
		return nil, fmt.Errorf("notification: 未知主题类型 %d", kind)
	}

	c := &Consumer{
		database:     opts.Database,
		contacts:     opts.Contacts,
		repo:         NewRepository(),
		logger:       opts.Logger,
		consumerName: name,
		emailEnabled: opts.EmailEnabled,
		kind:         kind,
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
		// 通知是派生数据，但邮件有外部副作用：全新消费组从最新位点开始，
		// 避免把保留期内的历史事件补发成邮件。已有位点的重启不受影响。
		Reset:     kafka.ResetLatest,
		RetryBase: opts.RetryBase,
		Security:  opts.Security,
	})
	if err != nil {
		return nil, err
	}
	c.consumer = kc
	return c, nil
}

func (c *Consumer) Run(ctx context.Context) error { return c.consumer.Run(ctx) }
func (c *Consumer) Close()                        { c.consumer.Close() }

// Handle 处理一条消息。分类规则与 report 消费者一致。
func (c *Consumer) Handle(ctx context.Context, msg kafka.Message) error {
	env, err := events.Decode(msg.Value)
	if err != nil {
		return kafka.Permanent(err)
	}

	switch c.kind {
	case topicExpense:
		return c.handleExpense(ctx, env)
	case topicBilling:
		return c.handleBilling(ctx, env)
	default:
		return kafka.Permanent(fmt.Errorf("notification: 未知主题类型 %d", c.kind))
	}
}

func (c *Consumer) handleExpense(ctx context.Context, env events.Envelope) error {
	snap, err := decodeExpenseEvent(env)
	if err != nil {
		return kafka.Permanent(err)
	}
	contacts, err := c.contacts.TenantContacts(ctx, env.TenantID)
	if err != nil {
		// identity 暂时不可用：不提交位点，退避后重试。
		return fmt.Errorf("notification: 获取收件人失败: %w", err)
	}
	drafts := draftsForExpense(env, snap, contacts, c.emailEnabled)
	return c.persist(ctx, env, drafts)
}

func (c *Consumer) handleBilling(ctx context.Context, env events.Envelope) error {
	// 先校验事件本身，再调用 identity：一条坏消息不应该产生一次外部请求，
	// 更不应该因为 identity 恰好不可用而被反复重试而不是进 DLQ。
	var render func([]Contact) []draft
	switch env.Type {
	case events.TypeSubscriptionCreated, events.TypeSubscriptionCanceled:
		snap, err := decodeSubscriptionEvent(env)
		if err != nil {
			return kafka.Permanent(err)
		}
		render = func(cs []Contact) []draft {
			return draftsForSubscription(env, snap, cs, c.emailEnabled)
		}
	case events.TypeInvoiceCreated, events.TypeInvoicePaid, events.TypeInvoicePaymentFailed:
		snap, err := decodeInvoiceEvent(env)
		if err != nil {
			return kafka.Permanent(err)
		}
		render = func(cs []Contact) []draft {
			return draftsForInvoice(env, snap, cs, c.emailEnabled)
		}
	default:
		// billing 主题只承载这五种事件；其他类型说明生产者或主题接错了。
		return kafka.Permanent(fmt.Errorf("notification: 不支持的事件类型 %q", env.Type))
	}

	contacts, err := c.contacts.TenantContacts(ctx, env.TenantID)
	if err != nil {
		return fmt.Errorf("notification: 获取收件人失败: %w", err)
	}
	return c.persist(ctx, env, render(contacts))
}

// persist 在租户事务内写幂等键、通知与邮件发件箱。
func (c *Consumer) persist(ctx context.Context, env events.Envelope, drafts []draft) error {
	var duplicate bool
	err := c.database.WithinTenantID(ctx, env.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		first, err := c.repo.markProcessed(ctx, tx, env.TenantID, c.consumerName, env.EventID)
		if err != nil {
			return err
		}
		if !first {
			duplicate = true
			return nil
		}
		for _, d := range drafts {
			if err := c.repo.insertNotification(ctx, tx, env.TenantID, d, env.EventID); err != nil {
				return fmt.Errorf("写入通知失败: %w", err)
			}
			if d.Email.DedupeKey != "" {
				if err := c.repo.insertEmail(ctx, tx, env.TenantID, d.Email); err != nil {
					return fmt.Errorf("写入邮件发件箱失败: %w", err)
				}
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return classifyDBError(err)
	}

	if duplicate {
		c.logger.Debug("duplicate notification event ignored",
			zap.String("event_id", env.EventID.String()),
			zap.String("event_type", string(env.Type)))
		return nil
	}
	c.logger.Info("notification event processed",
		zap.String("event_id", env.EventID.String()),
		zap.String("event_type", string(env.Type)),
		zap.Int("recipients", len(drafts)))
	return nil
}

// classifyDBError 与 report 消费者保持同一套判断：
// 数据异常/约束冲突/权限错误源自消息或表结构，重试不会改变结果。
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
