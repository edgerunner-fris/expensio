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
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

const (
	maxMerchantRunes    = 200  // 与 expenses_merchant_check 一致
	maxDescriptionRunes = 2000 // 与 expenses_description_check 一致
	// 允许少量时钟偏差：客户端比服务端快几十秒是常态，不能因此拒绝提交。
	maxOccurredSkew = 5 * time.Minute
)

// Service 承载 expense 的业务规则。事务边界在这里，不在 handler 也不在仓储。
type Service struct {
	database *db.DB
	repo     *Repository
	// now 可替换，测试用固定时钟验证"发生时间不能在未来"这类规则。
	now func() time.Time
}

func NewService(database *db.DB) *Service {
	return &Service{database: database, repo: NewRepository(), now: time.Now}
}

// DraftInput 是创建/修改草稿的请求体。
// 金额是整数最小单位：前端传"12.34 美元"时应当是 1234 + "USD"，而不是 12.34。
type DraftInput struct {
	Category    string     `json:"category"`
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"`
	Merchant    string     `json:"merchant"`
	Description string     `json:"description"`
	OccurredAt  time.Time  `json:"occurred_at"`
	BudgetID    *uuid.UUID `json:"budget_id,omitempty"`
}

// draftValues 是校验并归一化后的字段，只有它会被写入数据库。
type draftValues struct {
	category    Category
	currency    money.Currency
	amountMinor int64
	merchant    string
	description string
	occurredAt  time.Time
	budgetID    *uuid.UUID
}

// validate 做输入归一化与边界检查。所有规则都与数据库约束同源，
// 这里只是让非法输入在开事务之前就得到可读的 400。
func (in DraftInput) validate(now time.Time) (draftValues, error) {
	category, err := ParseCategory(in.Category)
	if err != nil {
		return draftValues{}, err
	}
	currency, err := money.ParseCurrency(in.Currency)
	if err != nil {
		return draftValues{}, err
	}
	if err := money.ValidateAmount(in.AmountMinor, currency); err != nil {
		return draftValues{}, err
	}

	merchant := strings.TrimSpace(in.Merchant)
	if utf8.RuneCountInString(merchant) > maxMerchantRunes {
		return draftValues{}, apperr.InvalidArgument("商家名称不超过 200 个字符")
	}
	description := strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(description) > maxDescriptionRunes {
		return draftValues{}, apperr.InvalidArgument("说明不超过 2000 个字符")
	}
	if in.OccurredAt.IsZero() {
		return draftValues{}, apperr.InvalidArgument("发生时间必填")
	}
	if in.OccurredAt.After(now.Add(maxOccurredSkew)) {
		return draftValues{}, apperr.InvalidArgument("发生时间不能晚于当前时间")
	}
	if in.BudgetID != nil && *in.BudgetID == uuid.Nil {
		return draftValues{}, apperr.InvalidArgument("预算 ID 非法")
	}

	return draftValues{
		category:    category,
		currency:    currency,
		amountMinor: in.AmountMinor,
		merchant:    merchant,
		description: description,
		occurredAt:  in.OccurredAt,
		budgetID:    in.BudgetID,
	}, nil
}

func (v draftValues) applyTo(e *Expense) {
	e.Category = v.category
	e.Currency = v.currency
	e.AmountMinor = v.amountMinor
	e.Merchant = v.merchant
	e.Description = v.description
	e.OccurredAt = v.occurredAt
	e.BudgetID = v.budgetID
}

// CreateDraft 新建一张草稿。tenant_id 只从 JWT 派生的上下文读取，
// 请求体里根本没有这个字段——不是"忽略"，而是不存在。
func (s *Service) CreateDraft(ctx context.Context, actor Actor, in DraftInput) (*Expense, error) {
	if !actor.valid() {
		return nil, apperr.Unauthorized("缺少有效的身份信息")
	}
	if !actor.Role.AtLeast(tenantctx.RoleMember) {
		return nil, apperr.Forbidden("只读成员不能创建费用单")
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}
	v, err := in.validate(s.now())
	if err != nil {
		return nil, err
	}

	e := &Expense{
		ID:          uuid.New(),
		TenantID:    tenantID,
		SubmitterID: actor.UserID,
		Status:      StatusDraft,
		Version:     1,
		CreatedBy:   actor.UserID,
		UpdatedBy:   actor.UserID,
	}
	v.applyTo(e)

	if err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.Create(ctx, tx, e)
	}); err != nil {
		return nil, mapDBError(err)
	}
	return e, nil
}

// UpdateDraft 修改草稿。expectedVersion 来自客户端读到的 version；
// 不一致说明有人先改了，返回 409 让前端刷新，而不是静默覆盖。
func (s *Service) UpdateDraft(ctx context.Context, actor Actor, id uuid.UUID, expectedVersion int, in DraftInput) (*Expense, error) {
	if expectedVersion <= 0 {
		return nil, apperr.InvalidArgument("version 必须是正整数")
	}
	v, err := in.validate(s.now())
	if err != nil {
		return nil, err
	}

	var out *Expense
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := AuthorizeDraftWrite(actor, e); err != nil {
			return err
		}
		v.applyTo(e)
		e.Version = expectedVersion
		e.UpdatedBy = actor.UserID
		if err := s.repo.UpdateDraft(ctx, tx, e); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrVersionConflict()
			}
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// DeleteDraft 软删草稿。删除也带版本号，避免"看到旧版本后删掉别的内容"。
func (s *Service) DeleteDraft(ctx context.Context, actor Actor, id uuid.UUID, expectedVersion int) error {
	if expectedVersion <= 0 {
		return apperr.InvalidArgument("version 必须是正整数")
	}
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := AuthorizeDraftWrite(actor, e); err != nil {
			return err
		}
		if err := s.repo.SoftDeleteDraft(ctx, tx, id, e.TenantID, actor.UserID, expectedVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrVersionConflict()
			}
			return err
		}
		return nil
	})
	return mapDBError(err)
}

// Get 读取单张费用单。看不到的单据一律返回 404：
// "存在但无权访问"和"不存在"对调用方应当是同一个答案，否则 ID 本身就成了探测接口。
func (s *Service) Get(ctx context.Context, actor Actor, id uuid.UUID) (*Expense, error) {
	var out *Expense
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		e, err := s.repo.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if !CanView(actor, e) {
			return apperr.NotFound("费用单")
		}
		out = e
		return nil
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// ListQuery 是列表请求。Scope 的空值等价于 mine。
type ListQuery struct {
	Scope  string
	Status string
	Limit  int
	Offset int
}

// List 返回分页结果与总数。分页参数在这里再收敛一次：
// handler 可能被复用，service 不能假设上游一定做过 Normalize。
func (s *Service) List(ctx context.Context, actor Actor, q ListQuery) ([]Expense, int64, error) {
	scope, err := ResolveScope(actor.Role, q.Scope)
	if err != nil {
		return nil, 0, err
	}
	var status Status
	if q.Status != "" {
		status = Status(q.Status)
		if !status.Valid() {
			return nil, 0, apperr.InvalidArgument("status 不是合法的费用单状态")
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	filter := ListFilter{Status: status, Limit: limit, Offset: offset}
	if scope == ScopeMine {
		filter.SubmitterID = actor.UserID
	}
	var (
		items []Expense
		total int64
	)
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		items, total, err = s.repo.List(ctx, tx, filter)
		return err
	})
	if err != nil {
		return nil, 0, mapDBError(err)
	}
	return items, total, nil
}

// mapDBError 把数据库错误翻译成对外错误。已经是业务错误的原样返回，
// 未知错误统一包成 500：根因只进日志，不进响应体。
func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return apperr.NotFound("费用单")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		// 复合外键同时拦住"预算不存在"和"预算是别的租户的"。
		return apperr.InvalidArgument("预算不存在或不属于当前租户")
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return apperr.Conflict(CodeStateConflict, "费用单当前状态不允许该操作")
	default:
		return apperr.Internal(err)
	}
}
