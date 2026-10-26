// 报表查询的输入模型、可见范围与取值校验。
//
// 聚合在查询时完成（M3 不建预聚合表）：事实表规模与租户账单量同阶，
// 预聚合会引入"什么时候刷新"的新问题，而查询侧有
// (tenant_id, occurred_at, category, currency) 索引可用。
package report

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Scope 是报表的可见范围。
type Scope string

const (
	ScopeMine Scope = "mine"
	ScopeAll  Scope = "all"
)

// GroupBy 是分组维度。只允许白名单里的列，SQL 里不做字符串拼接。
type GroupBy string

const (
	GroupByCategory GroupBy = "category"
	GroupByStatus   GroupBy = "status"
)

const (
	dateLayout   = "2006-01-02"
	maxRangeDays = 365
)

// SummaryQuery 是 GET /api/v1/reports/expenses/summary 的查询参数。
type SummaryQuery struct {
	Scope    string `form:"scope"`
	GroupBy  string `form:"group_by"`
	From     string `form:"from"`
	To       string `form:"to"`
	Status   string `form:"status"`
	Category string `form:"category"`
}

// CurrencyTotal 是按币种汇总。币种之间永远不求和。
type CurrencyTotal struct {
	Currency     string `json:"currency"`
	AmountMinor  int64  `json:"amount_minor"`
	ExpenseCount int64  `json:"expense_count"`
}

// GroupTotal 是分组 × 币种的汇总。每一行自带币种，
// 前端不可能把两行不同币种的金额相加而不自知。
type GroupTotal struct {
	Group        string `json:"group"`
	Currency     string `json:"currency"`
	AmountMinor  int64  `json:"amount_minor"`
	ExpenseCount int64  `json:"expense_count"`
}

// Summary 是响应体。空的汇总返回空数组而不是 null。
type Summary struct {
	Scope          Scope           `json:"scope"`
	GroupBy        GroupBy         `json:"group_by"`
	From           string          `json:"from"`
	To             string          `json:"to"`
	CurrencyTotals []CurrencyTotal `json:"currency_totals"`
	Groups         []GroupTotal    `json:"groups"`
}

// summaryFilter 是校验后的查询条件。
type summaryFilter struct {
	Scope       Scope
	GroupBy     GroupBy
	From        time.Time // 含
	ToExclusive time.Time // 不含：to 按自然日闭区间处理
	Status      string
	Category    string
	SubmitterID uuid.UUID // ScopeAll 时为零值
}

// 与数据库 CHECK 约束保持一致。
var summaryStatuses = map[string]struct{}{
	"submitted": {}, "approved": {}, "rejected": {}, "cancelled": {}, "reimbursed": {},
}

var summaryCategories = map[string]struct{}{
	"travel": {}, "meals": {}, "office": {}, "software": {}, "other": {},
}

// newSummaryFilter 校验并收敛所有查询参数。
func newSummaryFilter(actor tenantctx.Tenant, q SummaryQuery) (summaryFilter, error) {
	scope, err := resolveScope(actor, q.Scope)
	if err != nil {
		return summaryFilter{}, err
	}

	groupBy := GroupBy(q.GroupBy)
	switch groupBy {
	case "":
		groupBy = GroupByCategory
	case GroupByCategory, GroupByStatus:
	default:
		return summaryFilter{}, apperr.InvalidArgument("group_by 只能是 category 或 status")
	}

	if q.From == "" || q.To == "" {
		return summaryFilter{}, apperr.InvalidArgument("from 与 to 必填，格式为 YYYY-MM-DD")
	}
	from, err := time.Parse(dateLayout, q.From)
	if err != nil {
		return summaryFilter{}, apperr.InvalidArgument("from 格式必须是 YYYY-MM-DD")
	}
	to, err := time.Parse(dateLayout, q.To)
	if err != nil {
		return summaryFilter{}, apperr.InvalidArgument("to 格式必须是 YYYY-MM-DD")
	}
	if from.After(to) {
		return summaryFilter{}, apperr.InvalidArgument("from 不能晚于 to")
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		return summaryFilter{}, apperr.InvalidArgument(
			fmt.Sprintf("查询范围不能超过 %d 天", maxRangeDays))
	}

	if q.Status != "" {
		if _, ok := summaryStatuses[q.Status]; !ok {
			return summaryFilter{}, apperr.InvalidArgument("status 不是合法的费用单状态")
		}
	}
	if q.Category != "" {
		if _, ok := summaryCategories[q.Category]; !ok {
			return summaryFilter{}, apperr.InvalidArgument("category 不是合法的费用类别")
		}
	}

	f := summaryFilter{
		Scope:       scope,
		GroupBy:     groupBy,
		From:        from.UTC(),
		ToExclusive: to.AddDate(0, 0, 1).UTC(),
		Status:      q.Status,
		Category:    q.Category,
	}
	if scope == ScopeMine {
		f.SubmitterID = actor.UserID
	}
	return f, nil
}

// resolveScope 决定本次查询看谁的数据。
//
// 管理岗默认看全租户（报表的主要使用场景），成员与查看者默认只看自己；
// 非管理岗显式请求 all 返回 403 而不是静默降级——静默降级会让调用方
// 误以为拿到的是全租户数据。
func resolveScope(actor tenantctx.Tenant, requested string) (Scope, error) {
	if !actor.Role.Valid() || actor.UserID == uuid.Nil {
		return "", apperr.Unauthorized("缺少有效的身份信息")
	}
	switch Scope(requested) {
	case "":
		if actor.Role.CanManageMembers() {
			return ScopeAll, nil
		}
		return ScopeMine, nil
	case ScopeMine:
		return ScopeMine, nil
	case ScopeAll:
		if !actor.Role.CanManageMembers() {
			return "", apperr.Forbidden("无权查看全租户报表")
		}
		return ScopeAll, nil
	default:
		return "", apperr.InvalidArgument("scope 只能是 mine 或 all")
	}
}
