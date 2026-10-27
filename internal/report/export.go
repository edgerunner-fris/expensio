package report

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// maxExportRows 与 ADR-0012 一致：超过就要求缩小范围，不悄悄截断。
const maxExportRows = 50000

// ExportRow 是一行导出明细。字段全部来自读模型，不跨库查询。
type ExportRow struct {
	ExpenseID   uuid.UUID
	Status      string
	Category    string
	Currency    money.Currency
	AmountMinor int64
	OccurredAt  time.Time
	SubmitterID uuid.UUID
	DecidedBy   *uuid.UUID
	DecidedAt   *time.Time
	CreatedAt   time.Time
}

// ExportRows 返回当前筛选条件下的明细，最多 maxExportRows 行。
// 过滤条件与 summary 完全一致，避免"报表看到 10 行、导出却有 12 行"。
func (s *Service) ExportRows(ctx context.Context, actor tenantctx.Tenant, q SummaryQuery) ([]ExportRow, error) {
	filter, err := newSummaryFilter(actor, q)
	if err != nil {
		return nil, err
	}

	out := make([]ExportRow, 0, 256)
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT expense_id, status, category, currency, amount_minor,
			       occurred_at, submitter_id, decided_by, decided_at,
			       expense_created_at
			FROM app.expense_facts
			WHERE `+summaryWhere+`
			ORDER BY occurred_at, expense_id
			LIMIT $6`, append(filter.args(), maxExportRows+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var (
				row       ExportRow
				currency  string
				decidedBy pgtype.UUID
				decidedAt pgtype.Timestamptz
			)
			if err := rows.Scan(&row.ExpenseID, &row.Status, &row.Category, &currency,
				&row.AmountMinor, &row.OccurredAt, &row.SubmitterID,
				&decidedBy, &decidedAt, &row.CreatedAt); err != nil {
				return err
			}
			row.Currency = money.Currency(currency)
			if decidedBy.Valid {
				id := uuid.UUID(decidedBy.Bytes)
				row.DecidedBy = &id
			}
			if decidedAt.Valid {
				t := decidedAt.Time.UTC()
				row.DecidedAt = &t
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, mapQueryError(err)
	}
	if len(out) > maxExportRows {
		return nil, apperr.InvalidArgument(
			fmt.Sprintf("导出结果超过 %d 行，请缩小时间范围或增加过滤条件", maxExportRows))
	}
	return out, nil
}

var exportHeader = []string{
	"expense_id", "status", "category", "currency",
	"amount", "amount_minor", "occurred_at", "submitter_id",
	"decided_by", "decided_at", "created_at",
}

func csvValue(v *uuid.UUID) string {
	if v == nil {
		return ""
	}
	return v.String()
}

func csvTime(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.UTC().Format(time.RFC3339)
}

// ExportExpensesCSV 处理 GET /api/v1/reports/expenses.csv。
//
// 先取数再写响应头：查询失败仍能返回正常的 JSON 错误，而不是一个
// 已经开始传输、无法改状态码的半截 CSV。
func (h *Handler) ExportExpensesCSV(c *gin.Context) {
	actor, ok := tenantctx.From(c.Request.Context())
	if !ok {
		httpx.Error(c, apperr.Unauthorized("缺少访问令牌"))
		return
	}

	var q SummaryQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		httpx.Error(c, apperr.InvalidArgument("查询参数非法"))
		return
	}

	rows, err := h.svc.ExportRows(c.Request.Context(), actor, q)
	if err != nil {
		httpx.Error(c, err)
		return
	}

	filename := fmt.Sprintf("expenses-%s-%s.csv", q.From, q.To)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	// UTF-8 BOM：Excel 靠它识别中文表头；没有它，非 ASCII 会被当成本地编码。
	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF})

	w := csv.NewWriter(c.Writer)
	_ = w.Write(exportHeader)
	for i := range rows {
		row := &rows[i]
		_ = w.Write([]string{
			row.ExpenseID.String(),
			row.Status,
			row.Category,
			string(row.Currency),
			money.FormatNumber(row.AmountMinor, row.Currency),
			fmt.Sprintf("%d", row.AmountMinor),
			row.OccurredAt.UTC().Format(time.RFC3339),
			row.SubmitterID.String(),
			csvValue(row.DecidedBy),
			csvTime(row.DecidedAt),
			row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		// 响应已经开始传输，只能中止连接；客户端会看到不完整的文件。
		c.Error(err) //nolint:errcheck // gin 的 Error 只收集错误，不返回
		c.Abort()
	}
}
