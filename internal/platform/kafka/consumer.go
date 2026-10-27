// 包 kafka 的消费端：手动提交 + 失败分类 + DLQ。
//
// 语义是至少一次投递（at-least-once）：
//   - 业务处理成功并写完数据库后才提交位点；
//   - 瞬时错误原地重试，不提交位点，不丢消息；
//   - 永久错误（格式非法、违反契约）转投 DLQ 后再提交位点，
//     否则一条坏消息会永久卡住整个分区。
//
// 重复投递不可避免（提交失败、进程崩溃、再平衡都会导致重放），
// 因此 Handler 必须自己幂等——报表明细的去重落在 processed_events 表上。
package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

// Message 是交给业务处理的一条记录。字段刻意不直接用 *kgo.Record：
// 业务代码不应该能对客户端做任何操作。
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   []kgo.RecordHeader
}

// Handler 处理单条消息。返回 nil 表示已持久化，可以提交位点；
// 返回 Permanent(err) 表示重试没有意义，转入 DLQ；
// 其余错误一律按瞬时错误原地重试。
type Handler interface {
	Handle(ctx context.Context, msg Message) error
}

// HandlerFunc 让普通函数满足 Handler。
type HandlerFunc func(ctx context.Context, msg Message) error

func (f HandlerFunc) Handle(ctx context.Context, msg Message) error { return f(ctx, msg) }

// Publisher 是写 DLQ 所需的最小接口，由 *Producer 实现。
type Publisher interface {
	PublishRecord(ctx context.Context, rec *kgo.Record) error
}

// OffsetReset 决定消费组第一次出现（没有已提交位点）时从哪端开始。
// 报表需要回放历史事件重建读模型，通知则相反：全新部署不应把保留期内的
// 旧费用/账单事件补发成邮件。
type OffsetReset int

const (
	// ResetEarliest 是零值：从最早位点开始。
	ResetEarliest OffsetReset = iota
	// ResetLatest 从最新位点开始，只消费消费组成立之后写入的事件。
	ResetLatest
)

// ConsumerOptions 是消费端的全部外部依赖。
type ConsumerOptions struct {
	Brokers   []string
	GroupID   string
	Topic     string
	DLQTopic  string
	ClientID  string
	Handler   Handler
	Publisher Publisher
	Logger    *zap.Logger
	// Reset 只在消费组没有已提交位点时生效；已有位点不会被重置。
	Reset OffsetReset
	// RetryBase / RetryMax 控制瞬时错误的指数退避。
	RetryBase time.Duration
	RetryMax  time.Duration
}

// Consumer 是一个消费组的成员。
type Consumer struct {
	client    *kgo.Client
	handler   Handler
	publisher Publisher
	logger    *zap.Logger
	dlqTopic  string
	retryBase time.Duration
	retryMax  time.Duration
}

// NewConsumer 建立客户端。构造成功不代表 broker 可达：
// 消费循环会持续重连，Kafka 故障不应阻止服务启动。
func NewConsumer(opts ConsumerOptions) (*Consumer, error) {
	if len(opts.Brokers) == 0 {
		return nil, errors.New("kafka: brokers 不能为空")
	}
	if opts.GroupID == "" || opts.Topic == "" || opts.DLQTopic == "" {
		return nil, errors.New("kafka: groupID/topic/dlqTopic 不能为空")
	}
	if opts.ClientID == "" {
		return nil, errors.New("kafka: clientID 不能为空")
	}
	if opts.Handler == nil {
		return nil, errors.New("kafka: handler 不能为空")
	}
	if opts.Publisher == nil {
		return nil, errors.New("kafka: publisher 不能为空（DLQ 需要）")
	}
	if opts.Logger == nil {
		return nil, errors.New("kafka: logger 不能为空")
	}

	retryBase := opts.RetryBase
	if retryBase <= 0 {
		retryBase = 500 * time.Millisecond
	}
	retryMax := opts.RetryMax
	if retryMax <= 0 {
		retryMax = 30 * time.Second
	}
	if retryMax < retryBase {
		return nil, errors.New("kafka: retryMax 不能小于 retryBase")
	}

	reset := kgo.NewOffset().AtStart()
	if opts.Reset == ResetLatest {
		reset = kgo.NewOffset().AtEnd()
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(opts.Brokers...),
		kgo.ClientID(opts.ClientID),
		kgo.ConsumerGroup(opts.GroupID),
		kgo.ConsumeTopics(opts.Topic),
		// 位点只能由本包在业务提交之后显式提交，不能让后台自动提交
		// "抢跑"——那会把还没落库的消息标记为已消费。
		kgo.DisableAutoCommit(),
		// 新消费组的起点由调用方决定：报表回放历史，通知只看新事件。
		// 心跳由 franz-go 的后台协程维持，所以处理单条消息再久，
		// 只要进程活着就不会因为"没有及时 poll"被踢出消费组。
		kgo.ConsumeResetOffset(reset),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: 创建消费者失败: %w", err)
	}

	return &Consumer{
		client:    client,
		handler:   opts.Handler,
		publisher: opts.Publisher,
		logger:    opts.Logger,
		dlqTopic:  opts.DLQTopic,
		retryBase: retryBase,
		retryMax:  retryMax,
	}, nil
}

// Close 关闭客户端。应在 Run 返回之后调用。
func (c *Consumer) Close() { c.client.Close() }

// Run 阻塞消费，直到 ctx 取消。返回值只在不可恢复的内部错误时非 nil。
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("kafka consumer started")

	for {
		fetches := c.client.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}

		fetches.EachError(func(topic string, partition int32, err error) {
			c.logger.Warn("kafka fetch error",
				zap.String("topic", topic),
				zap.Int32("partition", partition),
				zap.Error(err))
		})

		records := fetches.Records()
		if len(records) == 0 {
			continue
		}

		for _, rec := range records {
			if err := c.process(ctx, rec); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}

		// 先完成业务与 DLQ，再提交位点。提交失败不会丢消息，只会重复投递。
		if err := c.client.CommitRecords(ctx, records...); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.logger.Error("kafka offset commit failed; records will be redelivered",
				zap.Error(err))
		}
	}
}

// process 处理一条记录，直到成功、转 DLQ 或 ctx 取消。
func (c *Consumer) process(ctx context.Context, rec *kgo.Record) error {
	msg := Message{
		Topic:     rec.Topic,
		Partition: rec.Partition,
		Offset:    rec.Offset,
		Key:       rec.Key,
		Value:     rec.Value,
		Headers:   rec.Headers,
	}

	// span 覆盖整个重试循环：一次处理尝试的耗时才是运维关心的，
	// 多次退避重试不应该被拆成互不相关的根 span。
	ctx, span := startConsume(ctx, msg)
	start := time.Now()
	outcome := "success"
	defer func() { finishConsume(ctx, span, msg.Topic, start, outcome) }()

	backoff := c.retryBase
	for {
		err := c.handler.Handle(ctx, msg)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// 关闭过程中不提交位点：消息会在下次启动时重放，
			// 这比把"处理到一半"当成已处理安全。
			outcome = "canceled"
			return ctx.Err()
		}

		if IsPermanent(err) {
			dlqErr := c.publishDLQ(ctx, msg, err)
			if dlqErr == nil {
				outcome = "dlq"
				c.logger.Error("kafka message routed to DLQ",
					zap.String("topic", msg.Topic),
					zap.Int32("partition", msg.Partition),
					zap.Int64("offset", msg.Offset),
					zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				outcome = "canceled"
				return ctx.Err()
			}
			err = fmt.Errorf("转投 DLQ 失败: %w", dlqErr)
		}

		c.logger.Warn("kafka message processing failed; retrying",
			zap.String("topic", msg.Topic),
			zap.Int32("partition", msg.Partition),
			zap.Int64("offset", msg.Offset),
			zap.Duration("backoff", backoff),
			zap.Error(err))

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			outcome = "canceled"
			return ctx.Err()
		case <-timer.C:
		}
		backoff = nextBackoff(backoff, c.retryMax)
	}
}

// publishDLQ 保留原始字节与键，附带来源位点和错误说明。
// 保留原样是为了以后可以不改代码直接重放。
func (c *Consumer) publishDLQ(ctx context.Context, msg Message, cause error) error {
	headers := make([]kgo.RecordHeader, 0, len(msg.Headers)+4)
	headers = append(headers, msg.Headers...)
	headers = append(headers,
		kgo.RecordHeader{Key: "x-original-topic", Value: []byte(msg.Topic)},
		kgo.RecordHeader{Key: "x-original-partition", Value: []byte(fmt.Sprint(msg.Partition))},
		kgo.RecordHeader{Key: "x-original-offset", Value: []byte(fmt.Sprint(msg.Offset))},
		kgo.RecordHeader{Key: "x-error", Value: []byte(cause.Error())},
	)
	return c.publisher.PublishRecord(ctx, &kgo.Record{
		Topic:   c.dlqTopic,
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	})
}

func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// permanentError 标记"重试也不会变好"的错误。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent 标记永久错误。重复标记幂等。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	if IsPermanent(err) {
		return err
	}
	return &permanentError{err: err}
}

// IsPermanent 判断错误是否被标记为永久。
func IsPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}
