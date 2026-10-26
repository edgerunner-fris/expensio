package report

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Repository 只读事实表。所有查询都在 WithinTenant 建立的事务里执行，
// RLS 会再挡一次；SQL 里的租户条件不是唯一防线。
type Repository struct{}

const summaryWhere = `
	occurred_at >= $1 AND occurred_at < $2
	AND ($3::text IS NULL OR status = $3)
	AND ($4::text IS NULL OR category = $4)
	AND ($5::uuid IS NULL OR submitter_id = $5)`

var groupColumns = map[GroupBy]string{
	GroupByCategory: "category",
	GroupByStatus:   "status",
}

// args 把可选条件转成 NULL 参数，避免把用户输入拼进 SQL。
func (f summaryFilter) args() []any {
	var status, category, submitter any
	if f.Status != "" {
		status = f.Status
	}
	if f.Category != "" {
		category = f.Category
	}
	if f.SubmitterID != uuid.Nil {
		submitter = f.SubmitterID
	}
	return []any{f.From, f.ToExclusive, status, category, submitter}
}

// Aggregate 一次查询同时得到币种总计与分组小计。
//
// 用 GROUPING SETS 而不是两次查询：单条语句看到的是同一个快照，
// 前端不会遇到"币种总计与分组之和对不上"。总计行只按 currency 分组，
// 因此任何金额都不会跨币种相加。
func (r *Repository) Aggregate(ctx context.Context, tx pgx.Tx, f summaryFilter) ([]CurrencyTotal, []GroupTotal, error) {
	// 列名只能来自白名单；用户输入永远进不了这段 SQL。
	column, ok := groupColumns[f.GroupBy]
	if !ok {
		return nil, nil, fmt.Errorf("report: 未知分组维度 %q", f.GroupBy)
	}

	query := fmt.Sprintf(`
		SELECT
		    CASE WHEN GROUPING(%[1]s) = 1 THEN NULL ELSE %[1]s END AS group_key,
		    currency,
		    sum(amount_minor)::bigint,
		    count(*)
		FROM app.expense_facts
		WHERE %[2]s
		GROUP BY GROUPING SETS ((%[1]s, currency), (currency))
		ORDER BY GROUPING(%[1]s), %[1]s, currency`, column, summaryWhere)

	rows, err := tx.Query(ctx, query, f.args()...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	totals := make([]CurrencyTotal, 0, 4)
	groups := make([]GroupTotal, 0, 8)
	for rows.Next() {
		var group pgtype.Text
		var currency string
		var amount, count int64
		if err := rows.Scan(&group, &currency, &amount, &count); err != nil {
			return nil, nil, err
		}
		if !group.Valid {
			totals = append(totals, CurrencyTotal{
				Currency:     currency,
				AmountMinor:  amount,
				ExpenseCount: count,
			})
			continue
		}
		groups = append(groups, GroupTotal{
			Group:        group.String,
			Currency:     currency,
			AmountMinor:  amount,
			ExpenseCount: count,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return totals, groups, nil
}
