package report

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// Service 提供报表查询。它读自己的库，不调用 expense 的 API。
type Service struct {
	database *db.DB
	repo     *Repository
}

func NewService(database *db.DB) *Service {
	return &Service{database: database, repo: &Repository{}}
}

// Summary 返回指定时间范围内的费用汇总。
func (s *Service) Summary(ctx context.Context, actor tenantctx.Tenant, q SummaryQuery) (Summary, error) {
	filter, err := newSummaryFilter(actor, q)
	if err != nil {
		return Summary{}, err
	}

	out := Summary{
		Scope:          filter.Scope,
		GroupBy:        filter.GroupBy,
		From:           q.From,
		To:             q.To,
		CurrencyTotals: []CurrencyTotal{},
		Groups:         []GroupTotal{},
	}

	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		totals, groups, err := s.repo.Aggregate(ctx, tx, filter)
		if err != nil {
			return err
		}
		out.CurrencyTotals = totals
		out.Groups = groups
		return nil
	})
	if err != nil {
		return Summary{}, mapQueryError(err)
	}
	return out, nil
}

// mapQueryError 保留业务错误，其余一律按内部错误处理：
// 查询参数已经校验过，剩下的数据库错误对调用方没有可操作信息。
func mapQueryError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.As(err); ok {
		return err
	}
	return apperr.Internal(err)
}
