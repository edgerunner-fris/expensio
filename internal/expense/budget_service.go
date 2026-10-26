package expense

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	maxBudgetNameRunes = 120 // 与 budgets_name_check 一致
	dateLayout         = "2006-01-02"
)

// BudgetInput 是创建预算的请求体。
// 期间用 YYYY-MM-DD 字符串而不是时间戳：预算按"日期"生效，
// 让客户端传带时区的时间戳会把"10 月 31 日"变成"10 月 30 日 16:00"。
type BudgetInput struct {
	Name        string `json:"name"`
	Category    string `json:"category"`
	Currency    string `json:"currency"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	AmountMinor int64  `json:"amount_minor"`
}

// BudgetUpdateInput 是修改预算的请求体。类别与币种不可改，因此不出现在这里。
type BudgetUpdateInput struct {
	Version     int    `json:"version"`
	Name        string `json:"name"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	AmountMinor int64  `json:"amount_minor"`
}

type budgetValues struct {
	name        string
	category    Category
	currency    money.Currency
	periodStart time.Time
	periodEnd   time.Time
	amountMinor int64
}

func parseBudgetPeriod(start, end string) (time.Time, time.Time, error) {
	from, err := time.Parse(dateLayout, strings.TrimSpace(start))
	if err != nil {
		return time.Time{}, time.Time{}, apperr.InvalidArgument("预算开始日期必须是 YYYY-MM-DD")
	}
	to, err := time.Parse(dateLayout, strings.TrimSpace(end))
	if err != nil {
		return time.Time{}, time.Time{}, apperr.InvalidArgument("预算结束日期必须是 YYYY-MM-DD")
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, apperr.InvalidArgument("预算结束日期不能早于开始日期")
	}
	return from, to, nil
}

func validateBudgetName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > maxBudgetNameRunes {
		return "", apperr.InvalidArgument("预算名称不能为空且不超过 120 个字符")
	}
	return name, nil
}

func (in BudgetInput) validate() (budgetValues, error) {
	name, err := validateBudgetName(in.Name)
	if err != nil {
		return budgetValues{}, err
	}
	category, err := ParseCategory(in.Category)
	if err != nil {
		return budgetValues{}, err
	}
	currency, err := money.ParseCurrency(in.Currency)
	if err != nil {
		return budgetValues{}, err
	}
	if err := money.ValidateAmount(in.AmountMinor, currency); err != nil {
		return budgetValues{}, err
	}
	from, to, err := parseBudgetPeriod(in.PeriodStart, in.PeriodEnd)
	if err != nil {
		return budgetValues{}, err
	}
	return budgetValues{
		name: name, category: category, currency: currency,
		periodStart: from, periodEnd: to, amountMinor: in.AmountMinor,
	}, nil
}

func (in BudgetUpdateInput) validate() (budgetValues, error) {
	name, err := validateBudgetName(in.Name)
	if err != nil {
		return budgetValues{}, err
	}
	if err := money.Validate(in.AmountMinor); err != nil {
		return budgetValues{}, err
	}
	from, to, err := parseBudgetPeriod(in.PeriodStart, in.PeriodEnd)
	if err != nil {
		return budgetValues{}, err
	}
	if in.Version <= 0 {
		return budgetValues{}, apperr.InvalidArgument("version 必须是正整数")
	}
	return budgetValues{name: name, periodStart: from, periodEnd: to, amountMinor: in.AmountMinor}, nil
}

// requireBudgetManager 限定预算写操作只能由 owner/admin 执行。
// 成员可以看到预算（提交时需要选预算），但不能调整额度。
func requireBudgetManager(actor Actor) error {
	if !actor.valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	if !actor.Role.CanManageMembers() {
		return apperr.Forbidden("只有管理员或所有者可以管理预算")
	}
	return nil
}

func (s *Service) CreateBudget(ctx context.Context, actor Actor, in BudgetInput) (*Budget, error) {
	if err := requireBudgetManager(actor); err != nil {
		return nil, err
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}
	v, err := in.validate()
	if err != nil {
		return nil, err
	}

	b := &Budget{
		ID:          uuid.New(),
		TenantID:    tenantID,
		Name:        v.name,
		Category:    v.category,
		Currency:    v.currency,
		PeriodStart: v.periodStart,
		PeriodEnd:   v.periodEnd,
		AmountMinor: v.amountMinor,
		CreatedBy:   actor.UserID,
		UpdatedBy:   actor.UserID,
	}
	if err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.CreateBudget(ctx, tx, b)
	}); err != nil {
		return nil, mapBudgetDBError(err)
	}
	return b, nil
}

func (s *Service) UpdateBudget(ctx context.Context, actor Actor, id uuid.UUID, in BudgetUpdateInput) (*Budget, error) {
	if err := requireBudgetManager(actor); err != nil {
		return nil, err
	}
	v, err := in.validate()
	if err != nil {
		return nil, err
	}

	var out *Budget
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := s.repo.GetBudget(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if b.Version != in.Version {
			return apperr.Conflict(CodeBudgetVersionConflict, "预算已被修改，请刷新后重试")
		}

		periodChanged := dateString(b.PeriodStart) != dateString(v.periodStart) ||
			dateString(b.PeriodEnd) != dateString(v.periodEnd)
		if periodChanged {
			used, err := s.repo.HasExpenses(ctx, tx, id)
			if err != nil {
				return err
			}
			if used {
				return apperr.Conflict(CodeBudgetInUse, "预算已被费用单引用，不能修改期间")
			}
		}
		if v.amountMinor < b.ReservedMinor+b.SpentMinor {
			return apperr.Conflict(CodeBudgetAmountBelowSpent, "预算金额不能小于已占用与已支出之和")
		}

		b.Name = v.name
		b.AmountMinor = v.amountMinor
		b.PeriodStart = v.periodStart
		b.PeriodEnd = v.periodEnd
		b.UpdatedBy = actor.UserID
		if err := s.repo.UpdateBudget(ctx, tx, b, in.Version); err != nil {
			if isNoRows(err) {
				return apperr.Conflict(CodeBudgetVersionConflict, "预算已被修改，请刷新后重试")
			}
			return err
		}
		out = b
		return nil
	})
	if err != nil {
		return nil, mapBudgetDBError(err)
	}
	return out, nil
}

func (s *Service) DeleteBudget(ctx context.Context, actor Actor, id uuid.UUID, version int) error {
	if err := requireBudgetManager(actor); err != nil {
		return err
	}
	if version <= 0 {
		return apperr.InvalidArgument("version 必须是正整数")
	}
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := s.repo.GetBudget(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if b.Version != version {
			return apperr.Conflict(CodeBudgetVersionConflict, "预算已被修改，请刷新后重试")
		}
		if b.ReservedMinor > 0 || b.SpentMinor > 0 {
			return apperr.Conflict(CodeBudgetInUse, "预算仍有占用或支出，不能删除")
		}
		if err := s.repo.SoftDeleteBudget(ctx, tx, id, b.TenantID, actor.UserID, version); err != nil {
			if isNoRows(err) {
				return apperr.Conflict(CodeBudgetVersionConflict, "预算已被修改，请刷新后重试")
			}
			return err
		}
		return nil
	})
	return mapBudgetDBError(err)
}

func (s *Service) GetBudget(ctx context.Context, actor Actor, id uuid.UUID) (*Budget, error) {
	if !actor.valid() {
		return nil, apperr.Unauthorized("缺少有效的身份信息")
	}
	var out *Budget
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		b, err := s.repo.GetBudget(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out = b
		return nil
	})
	if err != nil {
		return nil, mapBudgetDBError(err)
	}
	return out, nil
}

// BudgetQuery 是预算列表请求。
type BudgetQuery struct {
	Category string
	Limit    int
	Offset   int
}

func (s *Service) ListBudgets(ctx context.Context, actor Actor, q BudgetQuery) ([]Budget, int64, error) {
	if !actor.valid() {
		return nil, 0, apperr.Unauthorized("缺少有效的身份信息")
	}
	filter := BudgetFilter{Limit: clampLimit(q.Limit), Offset: clampOffset(q.Offset)}
	if q.Category != "" {
		category, err := ParseCategory(q.Category)
		if err != nil {
			return nil, 0, err
		}
		filter.Category = category
	}

	var (
		items []Budget
		total int64
	)
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, total, err = s.repo.ListBudgets(ctx, tx, filter)
		return err
	})
	if err != nil {
		return nil, 0, mapBudgetDBError(err)
	}
	return items, total, nil
}

// resolveBudget 决定提交时使用哪条预算。必须在审批事务内调用。
//
// 规则（2026-10-26 确认）：
//   - 指定了 budget_id：它必须存在、类别/币种一致、发生日期落在期间内；
//   - 未指定：按 (类别, 币种, 日期) 自动匹配唯一的一条；
//   - 匹配不到：租户已有预算 → 409（有预算就必须占用）；从未配置过 → 允许 NULL。
func (s *Service) resolveBudget(ctx context.Context, tx pgx.Tx, e *Expense) (*uuid.UUID, error) {
	if e.BudgetID != nil {
		b, err := s.repo.GetBudget(ctx, tx, *e.BudgetID, false)
		if err != nil {
			if isNoRows(err) {
				return nil, apperr.Conflict(CodeBudgetNotMatched, "指定的预算不存在或已删除")
			}
			return nil, err
		}
		if b.Category != e.Category || b.Currency != e.Currency ||
			!coversDate(b, e.OccurredAt) {
			return nil, apperr.Conflict(CodeBudgetNotMatched, "指定的预算与费用类别、币种或发生日期不匹配")
		}
		id := b.ID
		return &id, nil
	}

	b, err := s.repo.MatchBudget(ctx, tx, e.Category, e.Currency, e.OccurredAt)
	if err == nil {
		id := b.ID
		return &id, nil
	}
	if !isNoRows(err) {
		return nil, err
	}
	hasAny, err := s.repo.HasAnyBudget(ctx, tx)
	if err != nil {
		return nil, err
	}
	if hasAny {
		return nil, apperr.Conflict(CodeBudgetNotMatched,
			"没有匹配该费用类别、币种与发生日期的预算")
	}
	return nil, nil
}

func coversDate(b *Budget, at time.Time) bool {
	d := dateString(at.UTC())
	return d >= dateString(b.PeriodStart) && d <= dateString(b.PeriodEnd)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func mapBudgetDBError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	var pgErr *pgconn.PgError
	switch {
	case isNoRows(err):
		return apperr.NotFound("预算")
	case errors.As(err, &pgErr) && pgErr.Code == "23P01":
		return apperr.Conflict(CodeBudgetOverlap, "同类别、同币种的预算期间不能重叠")
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return apperr.Conflict(CodeBudgetInUse, "预算仍有占用或金额不满足约束")
	default:
		return apperr.Internal(err)
	}
}
