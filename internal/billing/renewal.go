package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
)

// 续费结果。日志与指标用同一组字符串，排障时不必在两套词汇之间翻译。
const (
	renewalRenewed   = "renewed"
	renewalSkipped   = "skipped"
	renewalNotActive = "not_active"
	renewalUpToDate  = "up_to_date"
	renewalUnpaid    = "unpaid"
)

const (
	defaultRenewalInterval = time.Hour
	defaultRenewalBatch    = 100
)

var (
	renewalResults = telemetry.NewCounter(
		"expensio.billing.renewal.results",
		"月度续费任务的处理结果",
		"{subscription}",
	)
	renewalErrors = telemetry.NewCounter(
		"expensio.billing.renewal.errors",
		"月度续费任务中单个订阅处理失败次数",
		"{subscription}",
	)
)

// RenewalCandidate 是到期目录返回的一行：只够定位，不构成任何业务判断。
type RenewalCandidate struct {
	SubscriptionID uuid.UUID
	TenantID       uuid.UUID
	PeriodEnd      time.Time
}

// RenewalReport 是一轮续费的汇总，供日志与测试断言。
type RenewalReport struct {
	// Due 是目录返回的候选数；Renewed + Skipped + Failed 应等于它。
	Due     int
	Renewed int
	Skipped int
	Failed  int
}

// RunRenewal 处理一批已到期的订阅。
//
// 目录查询本身在一个无租户上下文的事务里完成（FORCE RLS 下唯一能跨租户
// 读取的通道，见 migration 000003）；真正改动每个订阅时再进入该租户的
// 上下文并重新加锁复核。单个订阅失败只累计到 Failed，不中断整批——
// 一个租户的异常数据不应该阻塞其他租户续费。
func (s *Service) RunRenewal(ctx context.Context, limit int) (RenewalReport, error) {
	if limit <= 0 {
		limit = defaultRenewalBatch
	}
	asOf := dateOnly(s.now())

	var candidates []RenewalCandidate
	err := s.database.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		candidates, err = s.repo.ListDueSubscriptions(ctx, tx, asOf, limit)
		return err
	})
	if err != nil {
		return RenewalReport{}, err
	}

	report := RenewalReport{Due: len(candidates)}
	for _, c := range candidates {
		outcome, err := s.renewSubscription(ctx, c, asOf)
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.Failed++
			renewalErrors.Add(ctx, 1, attribute.String("reason", "error"))
			// 租户 ID 与订阅 ID 来自数据库目录，不是用户输入，可以安全入日志。
			s.logger.Warn("billing renewal: subscription failed",
				zap.String("tenant_id", c.TenantID.String()),
				zap.String("subscription_id", c.SubscriptionID.String()),
				zap.Error(err))
			continue
		}
		renewalResults.Add(ctx, 1, attribute.String("outcome", outcome))
		if outcome == renewalRenewed {
			report.Renewed++
		} else {
			report.Skipped++
		}
	}
	return report, nil
}

// renewSubscription 在一个租户事务里完成续费。
//
// 返回的 outcome 说明这行为什么被跳过：目录是异步维护的，候选可能已经
// 取消、已经续过，或上一张账单还没付。只有 renewed 会写账单与 outbox。
func (s *Service) renewSubscription(ctx context.Context, c RenewalCandidate, asOf time.Time) (string, error) {
	outcome := renewalUpToDate
	err := s.database.WithinTenantID(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sub, err := s.repo.GetSubscription(ctx, tx, c.SubscriptionID, true)
		if err != nil {
			if isNoRows(err) {
				// 目录行过期（订阅已被删除或不可见）。跳过即可，
				// 这不是错误，也不应该让整批失败。
				return nil
			}
			return err
		}
		if sub.Status != SubscriptionActive {
			outcome = renewalNotActive
			return nil
		}
		if !sub.CurrentPeriodEnd.Before(asOf) {
			// 另一个实例已经续过这一期。
			outcome = renewalUpToDate
			return nil
		}
		unpaid, err := s.repo.HasOpenInvoice(ctx, tx, sub.ID)
		if err != nil {
			return err
		}
		if unpaid {
			outcome = renewalUnpaid
			return nil
		}

		plan, err := s.repo.GetPlan(ctx, tx, sub.PlanCode)
		if err != nil {
			return err
		}
		if err := plan.Validate(); err != nil {
			return err
		}
		amount, err := InvoiceAmount(*plan, sub.Seats)
		if err != nil {
			return err
		}
		nextStart, err := NextPeriodStart(sub.CurrentPeriodStart, plan.Interval, sub.AnchorDay)
		if err != nil {
			return err
		}
		nextEnd, err := NextPeriodStart(nextStart, plan.Interval, sub.AnchorDay)
		if err != nil {
			return err
		}
		nextEnd = nextEnd.AddDate(0, 0, -1)

		now := s.now().UTC()
		due := dateOnly(now).AddDate(0, 0, invoiceDueDays)
		if due.Before(nextStart) {
			due = nextStart
		}
		inv := &Invoice{
			ID:             uuid.New(),
			TenantID:       sub.TenantID,
			SubscriptionID: sub.ID,
			Status:         InvoiceOpen,
			PeriodStart:    nextStart,
			PeriodEnd:      nextEnd,
			Currency:       plan.Currency,
			AmountMinor:    amount,
			DueOn:          due,
		}
		inv.Number = invoiceNumber(now, inv.ID)
		if err := s.repo.CreateInvoice(ctx, tx, inv); err != nil {
			return err
		}
		if err := s.repo.RolloverSubscription(ctx, tx, sub, nextStart, nextEnd); err != nil {
			return err
		}
		// 事件在周期前移之后构造：账单快照本身不含订阅版本，但同一事务里
		// 先写账单再改订阅可以让唯一约束与 guard 的报错都落在本事务内。
		env, err := newInvoiceEvent(events.TypeInvoiceCreated, inv, inv.IssuedAt)
		if err != nil {
			return err
		}
		if err := s.outbox.Insert(ctx, tx, events.TopicBilling, env); err != nil {
			return err
		}
		outcome = renewalRenewed
		return nil
	})
	if err != nil {
		return "", mapBillingDBError(err)
	}
	return outcome, nil
}

// RenewalJob 按固定间隔调用 Service.RunRenewal。
//
// 多副本同时运行是安全的：每个订阅的续费在行锁内复核"是否仍然到期"，
// 后到的实例看到已经前移的周期直接跳过。因此不需要额外的分布式锁。
type RenewalJob struct {
	svc      *Service
	logger   *zap.Logger
	interval time.Duration
	batch    int
}

func NewRenewalJob(svc *Service, logger *zap.Logger, interval time.Duration, batch int) (*RenewalJob, error) {
	if svc == nil {
		return nil, errors.New("billing: 续费任务需要 Service")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = defaultRenewalInterval
	}
	if batch <= 0 {
		batch = defaultRenewalBatch
	}
	return &RenewalJob{svc: svc, logger: logger, interval: interval, batch: batch}, nil
}

// Run 阻塞运行直到 ctx 取消。启动时先跑一轮：重启后的第一小时不应该
// 留着一批已到期的订阅不处理。
func (j *RenewalJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()

	j.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

func (j *RenewalJob) tick(ctx context.Context) {
	started := time.Now()
	report, err := j.svc.RunRenewal(ctx, j.batch)
	if err != nil {
		if ctx.Err() == nil {
			j.logger.Error("billing renewal run failed", zap.Error(err))
		}
		return
	}
	if report.Due == 0 {
		j.logger.Debug("billing renewal: nothing due")
		return
	}
	j.logger.Info("billing renewal run finished",
		zap.Int("due", report.Due),
		zap.Int("renewed", report.Renewed),
		zap.Int("skipped", report.Skipped),
		zap.Int("failed", report.Failed),
		zap.Duration("elapsed", time.Since(started)),
	)
}
