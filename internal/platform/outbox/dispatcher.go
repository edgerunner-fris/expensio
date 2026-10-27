package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
)

const (
	defaultBatchSize      = 50
	defaultPollInterval   = time.Second
	defaultLease          = 30 * time.Second
	defaultPublishTimeout = 12 * time.Second
	maxBackoff            = time.Minute
	bookkeepingTimeout    = 5 * time.Second
)

var (
	outboxClaimed = telemetry.NewCounter(
		"expensio.outbox.events.claimed",
		"投递器每批从 outbox 领取的事件数",
		"{event}",
	)
	outboxDelivered = telemetry.NewCounter(
		"expensio.outbox.events.delivered",
		"outbox 事件投递结果（成功/失败）",
		"{event}",
	)
)

// Publisher 是投递器依赖的最小接口。用接口而不是直接拿 Kafka 客户端，
// 是为了能在不启动消息系统的前提下测试重试与标记逻辑。
type Publisher interface {
	Publish(ctx context.Context, topic, key string, value []byte) error
}

// DispatcherOptions 是投递器的可调参数；零值字段会被默认值补上。
type DispatcherOptions struct {
	Database *db.DB
	Store    *Store
	// Publisher 必填；为 nil 时构造失败。
	Publisher Publisher
	Logger    *zap.Logger

	BatchSize      int
	PollInterval   time.Duration
	Lease          time.Duration
	PublishTimeout time.Duration
	// Now 仅用于测试退避时间；生产传 nil 即使用 time.Now。
	Now func() time.Time
}

// Dispatcher 把 outbox 中待发的事件投递到 Kafka。
//
// 领取与标记各用一个短事务；发送发生在两个事务之间。
// 绝不在数据库事务内等待 broker——Kafka 慢或不可用时，
// 报销接口不能被一起拖死。
type Dispatcher struct {
	database *db.DB
	store    *Store
	pub      Publisher
	logger   *zap.Logger

	batchSize      int
	pollInterval   time.Duration
	lease          time.Duration
	publishTimeout time.Duration
	now            func() time.Time
}

func NewDispatcher(opts DispatcherOptions) (*Dispatcher, error) {
	if opts.Database == nil {
		return nil, errors.New("outbox: Dispatcher 需要数据库")
	}
	if opts.Publisher == nil {
		return nil, errors.New("outbox: Dispatcher 需要 Publisher")
	}
	d := &Dispatcher{
		database:       opts.Database,
		store:          opts.Store,
		pub:            opts.Publisher,
		logger:         opts.Logger,
		batchSize:      opts.BatchSize,
		pollInterval:   opts.PollInterval,
		lease:          opts.Lease,
		publishTimeout: opts.PublishTimeout,
		now:            opts.Now,
	}
	if d.store == nil {
		d.store = NewStore()
	}
	if d.logger == nil {
		d.logger = zap.NewNop()
	}
	if d.batchSize <= 0 || d.batchSize > 100 {
		d.batchSize = defaultBatchSize
	}
	if d.pollInterval <= 0 {
		d.pollInterval = defaultPollInterval
	}
	if d.lease <= 0 {
		d.lease = defaultLease
	}
	if d.publishTimeout <= 0 {
		d.publishTimeout = defaultPublishTimeout
	}
	if d.now == nil {
		d.now = time.Now
	}
	return d, nil
}

// Run 持续投递，直到 ctx 被取消。取消后不再领取新事件，
// 正在发送的一条会带着取消信号结束，并按下一次重试留痕。
func (d *Dispatcher) Run(ctx context.Context) {
	d.logger.Info("outbox dispatcher started",
		zap.Int("batch_size", d.batchSize),
		zap.Duration("poll_interval", d.pollInterval))
	for {
		n, err := d.DispatchOnce(ctx)
		if err != nil && ctx.Err() == nil {
			d.logger.Warn("outbox claim failed", zap.Error(err))
		}
		if n >= d.batchSize {
			continue // 可能还有积压，不等待直接再取一批
		}

		timer := time.NewTimer(d.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			d.logger.Info("outbox dispatcher stopped")
			return
		case <-timer.C:
		}
	}
}

// DispatchOnce 领取并投递一批事件，返回领取条数。
// 单条事件的投递失败会写回 outbox 并继续处理同批其他事件；
// 只有领取本身失败才返回 error。
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	var batch []Event
	err := d.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		batch, err = d.store.Claim(ctx, tx, d.batchSize, d.lease)
		return err
	})
	if err != nil {
		return 0, err
	}
	outboxClaimed.Add(ctx, int64(len(batch)))

	published := 0
	for _, ev := range batch {
		if err := d.publishOne(ctx, ev); err != nil {
			d.recordFailure(ctx, ev, err)
			continue
		}
		d.recordSuccess(ctx, ev)
		published++
	}
	if len(batch) > 0 {
		d.logger.Info("outbox batch processed",
			zap.Int("claimed", len(batch)),
			zap.Int("published", published))
	}
	return len(batch), nil
}

func (d *Dispatcher) publishOne(ctx context.Context, ev Event) error {
	pubCtx, cancel := context.WithTimeout(ctx, d.publishTimeout)
	defer cancel()
	return d.pub.Publish(pubCtx, ev.Topic, ev.TenantID.String(), ev.Payload)
}

func (d *Dispatcher) recordSuccess(ctx context.Context, ev Event) {
	outboxDelivered.Add(ctx, 1,
		attribute.String("event_type", ev.Type),
		attribute.String("outcome", "success"))
	d.withBookkeeping(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return d.store.MarkPublished(ctx, tx, ev.ID)
	}, "mark published failed", ev)
}

func (d *Dispatcher) recordFailure(ctx context.Context, ev Event, cause error) {
	outboxDelivered.Add(ctx, 1,
		attribute.String("event_type", ev.Type),
		attribute.String("outcome", "error"))
	// 已经失败的次数从 attempts 读出；本次是第 attempts+1 次失败。
	retryAt := d.now().Add(Backoff(ev.Attempts + 1))
	d.withBookkeeping(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return d.store.MarkFailed(ctx, tx, ev.ID, cause, retryAt)
	}, "mark failed delivery failed", ev)
	d.logger.Warn("outbox publish failed",
		zap.String("event_id", ev.ID.String()),
		zap.String("tenant_id", ev.TenantID.String()),
		zap.String("event_type", ev.Type),
		zap.Int("attempt", ev.Attempts+1),
		zap.Time("retry_at", retryAt),
		zap.Error(cause))
}

// withBookkeeping 用与请求取消无关的短上下文写状态：
// 进程关闭时正在发送的一条即使被取消，也要把失败原因落库。
func (d *Dispatcher) withBookkeeping(ctx context.Context, fn func(context.Context, pgx.Tx) error, msg string, ev Event) {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()
	if err := d.database.WithTx(markCtx, fn); err != nil {
		d.logger.Error(msg,
			zap.String("event_id", ev.ID.String()),
			zap.Error(err))
	}
}

// Backoff 返回第 attempts 次失败后的等待时间：1s → 2s → 4s …，上限 1m。
// attempts 从 1 开始计数。
func Backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Second
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}
