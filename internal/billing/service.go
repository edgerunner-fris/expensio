package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 错误码供前端区分"已有订阅""版本冲突"等可操作状态。
const (
	CodeSubscriptionExists          = "billing.subscription_exists"
	CodeSubscriptionStateConflict   = "billing.subscription_state_conflict"
	CodeSubscriptionVersionConflict = "billing.subscription_version_conflict"
)

// Service 承载订阅的业务规则：谁能改、一个租户一条生效订阅、取消是终态。
type Service struct {
	database *db.DB
	repo     *Repository
	now      func() time.Time
}

func NewService(database *db.DB) *Service {
	return &Service{database: database, repo: NewRepository(), now: time.Now}
}

// SubscriptionInput 是开通订阅的请求体。
// 租户不在请求体里：它只来自 JWT（见 tenantctx）。
type SubscriptionInput struct {
	PlanCode string `json:"plan_code"`
	Seats    int    `json:"seats"`
}

func requireMember(actor tenantctx.Tenant) error {
	if actor.UserID == uuid.Nil || !actor.Role.Valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	return nil
}

func requireBillingManager(actor tenantctx.Tenant) error {
	if err := requireMember(actor); err != nil {
		return err
	}
	if !CanManageBilling(actor.Role) {
		return apperr.Forbidden("只有管理员或所有者可以管理订阅")
	}
	return nil
}

func (in SubscriptionInput) validate() (string, error) {
	code := strings.TrimSpace(in.PlanCode)
	if code == "" {
		return "", apperr.InvalidArgument("套餐代码不能为空")
	}
	if in.Seats < MinSeats || in.Seats > MaxSeats {
		return "", apperr.InvalidArgument("席位数必须在 1 到 1000 之间")
	}
	return code, nil
}

// ListPlans 返回全局套餐目录，所有已认证成员都可读。
func (s *Service) ListPlans(ctx context.Context, actor tenantctx.Tenant) ([]Plan, error) {
	if err := requireMember(actor); err != nil {
		return nil, err
	}
	var out []Plan
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.ListPlans(ctx, tx)
		return err
	})
	if err != nil {
		return nil, mapBillingDBError(err)
	}
	return out, nil
}

// CreateSubscription 开通订阅：周期从今天（UTC）开始，按套餐周期推算结束日。
// 是否已有生效订阅由部分唯一索引判定，不做先查后插。
func (s *Service) CreateSubscription(ctx context.Context, actor tenantctx.Tenant, in SubscriptionInput) (*Subscription, error) {
	if err := requireBillingManager(actor); err != nil {
		return nil, err
	}
	code, err := in.validate()
	if err != nil {
		return nil, err
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}

	var out *Subscription
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		plan, err := s.repo.GetPlan(ctx, tx, code)
		if err != nil {
			if isNoRows(err) {
				return apperr.InvalidArgument("套餐不存在")
			}
			return err
		}
		if err := plan.Validate(); err != nil {
			return err
		}
		start := dateOnly(s.now())
		end, err := PeriodEnd(start, plan.Interval)
		if err != nil {
			return err
		}
		sub := &Subscription{
			ID:                 uuid.New(),
			TenantID:           tenantID,
			PlanCode:           plan.Code,
			Seats:              in.Seats,
			Status:             SubscriptionActive,
			CurrentPeriodStart: start,
			CurrentPeriodEnd:   end,
			CreatedBy:          actor.UserID,
			UpdatedBy:          actor.UserID,
		}
		if err := s.repo.CreateSubscription(ctx, tx, sub); err != nil {
			return err
		}
		out = sub
		return nil
	})
	if err != nil {
		return nil, mapBillingDBError(err)
	}
	return out, nil
}

func (s *Service) GetSubscription(ctx context.Context, actor tenantctx.Tenant, id uuid.UUID) (*Subscription, error) {
	if err := requireMember(actor); err != nil {
		return nil, err
	}
	var out *Subscription
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.GetSubscription(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return nil, mapBillingDBError(err)
	}
	return out, nil
}

// GetCurrentSubscription 返回当前生效订阅；从未订阅过或已取消时返回 404。
func (s *Service) GetCurrentSubscription(ctx context.Context, actor tenantctx.Tenant) (*Subscription, error) {
	if err := requireMember(actor); err != nil {
		return nil, err
	}
	var out *Subscription
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.GetLiveSubscription(ctx, tx)
		return err
	})
	if err != nil {
		return nil, mapBillingDBError(err)
	}
	return out, nil
}

func (s *Service) ListSubscriptions(ctx context.Context, actor tenantctx.Tenant, limit, offset int) ([]Subscription, int64, error) {
	if err := requireMember(actor); err != nil {
		return nil, 0, err
	}
	var (
		items []Subscription
		total int64
	)
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, total, err = s.repo.ListSubscriptions(ctx, tx, limit, offset)
		return err
	})
	if err != nil {
		return nil, 0, mapBillingDBError(err)
	}
	return items, total, nil
}

// CancelSubscription 取消订阅。取消是终态，重复取消返回 409，
// 而不是把 canceled_at 越改越晚。
func (s *Service) CancelSubscription(ctx context.Context, actor tenantctx.Tenant, id uuid.UUID, version int) (*Subscription, error) {
	if err := requireBillingManager(actor); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, apperr.InvalidArgument("version 必须是正整数")
	}

	var out *Subscription
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sub, err := s.repo.GetSubscription(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if sub.Version != version {
			return apperr.Conflict(CodeSubscriptionVersionConflict, "订阅已被修改，请刷新后重试")
		}
		if !sub.Status.CanTransition(SubscriptionCanceled) {
			return apperr.Conflict(CodeSubscriptionStateConflict, "当前订阅状态不能取消")
		}
		now := s.now().UTC()
		sub.CanceledAt = &now
		sub.UpdatedBy = actor.UserID
		if err := s.repo.CancelSubscription(ctx, tx, sub, version); err != nil {
			if isNoRows(err) {
				return apperr.Conflict(CodeSubscriptionVersionConflict, "订阅已被修改，请刷新后重试")
			}
			return err
		}
		out = sub
		return nil
	})
	if err != nil {
		return nil, mapBillingDBError(err)
	}
	return out, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func mapBillingDBError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	var pgErr *pgconn.PgError
	switch {
	case isNoRows(err):
		return apperr.NotFound("订阅")
	case errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "subscriptions_one_live_per_tenant":
		return apperr.Conflict(CodeSubscriptionExists, "该租户已有生效中的订阅")
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return apperr.Conflict(CodeSubscriptionVersionConflict, "订阅已被修改，请刷新后重试")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return apperr.InvalidArgument("套餐不存在")
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return apperr.Conflict(CodeSubscriptionStateConflict, "订阅状态不满足约束")
	default:
		return apperr.Internal(err)
	}
}
