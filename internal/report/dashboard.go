package report

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// DashboardQuery 是 GET /api/v1/reports/dashboard 的查询参数。
// 过滤条件与 summary 保持同一套语义，便于前端在同一页切换。
type DashboardQuery struct {
	Scope string `form:"scope"`
	From  string `form:"from"`
	To    string `form:"to"`
}

// StatusTotal 是状态 × 币种的小计。
type StatusTotal struct {
	Status       string `json:"status"`
	Currency     string `json:"currency"`
	AmountMinor  int64  `json:"amount_minor"`
	ExpenseCount int64  `json:"expense_count"`
}

// DailyTotal 是发生日（UTC）× 币种的小计。日期用字符串，
// 避免前端把 "2026-10-01" 解析成带时区的本地时间后偏移一天。
type DailyTotal struct {
	Date         string `json:"date"`
	Currency     string `json:"currency"`
	AmountMinor  int64  `json:"amount_minor"`
	ExpenseCount int64  `json:"expense_count"`
}

// Dashboard 是仪表盘响应。所有数组在空结果时也是 []，不是 null。
type Dashboard struct {
	Scope          Scope           `json:"scope"`
	From           string          `json:"from"`
	To             string          `json:"to"`
	CurrencyTotals []CurrencyTotal `json:"currency_totals"`
	StatusTotals   []StatusTotal   `json:"status_totals"`
	CategoryTotals []GroupTotal    `json:"category_totals"`
	DailyTotals    []DailyTotal    `json:"daily_totals"`
}

// newDashboardFilter 复用 summary 的校验与范围规则。
// 仪表盘不开放 status/category 过滤：它是概览，不是报表的另一种形态。
func newDashboardFilter(actor tenantctx.Tenant, q DashboardQuery) (summaryFilter, error) {
	return newSummaryFilter(actor, SummaryQuery{
		Scope: q.Scope,
		From:  q.From,
		To:    q.To,
	})
}

// Dashboard 聚合仪表盘数据。四条查询在同一个事务里执行，
// 因此卡片、状态、类别与趋势看到的是同一个快照。
func (s *Service) Dashboard(ctx context.Context, actor tenantctx.Tenant, q DashboardQuery) (Dashboard, error) {
	filter, err := newDashboardFilter(actor, q)
	if err != nil {
		return Dashboard{}, err
	}

	// 缓存键包含提交人：scope=mine 时每个成员各自一份，绝不共用。
	cached, key, hit := s.cache.Lookup(ctx, actor.ID, filter.SubmitterID, filter.Scope, q.From, q.To)
	if hit {
		return cached, nil
	}

	out := Dashboard{
		Scope:          filter.Scope,
		From:           q.From,
		To:             q.To,
		CurrencyTotals: []CurrencyTotal{},
		StatusTotals:   []StatusTotal{},
		CategoryTotals: []GroupTotal{},
		DailyTotals:    []DailyTotal{},
	}

	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if out.CurrencyTotals, err = s.repo.CurrencyTotals(ctx, tx, filter); err != nil {
			return err
		}
		if out.StatusTotals, err = s.repo.StatusTotals(ctx, tx, filter); err != nil {
			return err
		}
		if out.CategoryTotals, err = s.repo.CategoryTotals(ctx, tx, filter); err != nil {
			return err
		}
		out.DailyTotals, err = s.repo.DailyTotals(ctx, tx, filter)
		return err
	})
	if err != nil {
		return Dashboard{}, mapQueryError(err)
	}
	s.cache.Store(ctx, key, out)
	return out, nil
}

func (r *Repository) CurrencyTotals(ctx context.Context, tx pgx.Tx, f summaryFilter) ([]CurrencyTotal, error) {
	rows, err := tx.Query(ctx, `
		SELECT currency, sum(amount_minor)::bigint, count(*)
		FROM app.expense_facts
		WHERE `+summaryWhere+`
		GROUP BY currency
		ORDER BY currency`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]CurrencyTotal, 0, 4)
	for rows.Next() {
		var t CurrencyTotal
		if err := rows.Scan(&t.Currency, &t.AmountMinor, &t.ExpenseCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) StatusTotals(ctx context.Context, tx pgx.Tx, f summaryFilter) ([]StatusTotal, error) {
	rows, err := tx.Query(ctx, `
		SELECT status, currency, sum(amount_minor)::bigint, count(*)
		FROM app.expense_facts
		WHERE `+summaryWhere+`
		GROUP BY status, currency
		ORDER BY status, currency`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StatusTotal, 0, 8)
	for rows.Next() {
		var t StatusTotal
		if err := rows.Scan(&t.Status, &t.Currency, &t.AmountMinor, &t.ExpenseCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) CategoryTotals(ctx context.Context, tx pgx.Tx, f summaryFilter) ([]GroupTotal, error) {
	rows, err := tx.Query(ctx, `
		SELECT category, currency, sum(amount_minor)::bigint, count(*)
		FROM app.expense_facts
		WHERE `+summaryWhere+`
		GROUP BY category, currency
		ORDER BY category, currency`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]GroupTotal, 0, 8)
	for rows.Next() {
		var t GroupTotal
		if err := rows.Scan(&t.Group, &t.Currency, &t.AmountMinor, &t.ExpenseCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) DailyTotals(ctx context.Context, tx pgx.Tx, f summaryFilter) ([]DailyTotal, error) {
	rows, err := tx.Query(ctx, `
		SELECT to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
		       currency, sum(amount_minor)::bigint, count(*)
		FROM app.expense_facts
		WHERE `+summaryWhere+`
		GROUP BY day, currency
		ORDER BY day, currency`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DailyTotal, 0, 32)
	for rows.Next() {
		var t DailyTotal
		if err := rows.Scan(&t.Date, &t.Currency, &t.AmountMinor, &t.ExpenseCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Dashboard 处理 GET /api/v1/reports/dashboard。
func (h *Handler) Dashboard(c *gin.Context) {
	actor, ok := tenantctx.From(c.Request.Context())
	if !ok {
		httpx.Error(c, apperr.Unauthorized("缺少访问令牌"))
		return
	}

	var q DashboardQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}

	out, err := h.svc.Dashboard(c.Request.Context(), actor, q)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, out)
}
