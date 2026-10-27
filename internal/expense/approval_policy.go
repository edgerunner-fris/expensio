package expense

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// PolicyInput 是设置二级审批门槛的请求体。
// 门槛以最小货币单位表达，并与费用单金额同一币种比较。
type PolicyInput struct {
	SecondLevelThresholdMinor int64 `json:"second_level_threshold_minor"`
}

// ListPolicies 返回当前租户的审批策略；只有管理岗能看。
func (s *Service) ListPolicies(ctx context.Context, actor Actor) ([]ApprovalPolicy, error) {
	if err := requireManage(actor); err != nil {
		return nil, err
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}

	var out []ApprovalPolicy
	err := s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = s.repo.ListPolicies(ctx, tx, tenantID)
		return err
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return out, nil
}

// UpsertPolicy 新建或更新某币种的门槛。策略按自然键 upsert，
// 因此重复保存同一币种不会产生多条互相矛盾的策略。
func (s *Service) UpsertPolicy(ctx context.Context, actor Actor, rawCurrency string, in PolicyInput) (*ApprovalPolicy, error) {
	if err := requireManage(actor); err != nil {
		return nil, err
	}
	currency, err := money.ParseCurrency(rawCurrency)
	if err != nil {
		return nil, err
	}
	if err := money.Validate(in.SecondLevelThresholdMinor); err != nil {
		return nil, apperr.InvalidArgument("二级审批门槛必须大于 0 且不超过允许范围")
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return nil, apperr.Unauthorized("缺少租户上下文")
	}

	p := &ApprovalPolicy{
		TenantID:                  tenantID,
		Currency:                  currency,
		SecondLevelThresholdMinor: in.SecondLevelThresholdMinor,
		CreatedBy:                 actor.UserID,
		UpdatedBy:                 actor.UserID,
	}
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = s.repo.UpsertPolicy(ctx, tx, p)
		return err
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	return p, nil
}

// DeletePolicy 取消某币种的分级审批。删除不存在策略也返回成功：
// 调用方要的结果是"该币种不再分级"，这个结果已经成立。
func (s *Service) DeletePolicy(ctx context.Context, actor Actor, rawCurrency string) error {
	if err := requireManage(actor); err != nil {
		return err
	}
	currency, err := money.ParseCurrency(rawCurrency)
	if err != nil {
		return err
	}
	tenantID := tenantctx.ID(ctx)
	if tenantID == uuid.Nil {
		return apperr.Unauthorized("缺少租户上下文")
	}
	err = s.database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.repo.DeletePolicy(ctx, tx, tenantID, string(currency))
	})
	return mapDBError(err)
}

func requireManage(actor Actor) error {
	if !actor.valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	if !actor.manage() {
		return apperr.Forbidden("只有管理员或所有者可以管理审批策略")
	}
	return nil
}

func (r *Repository) ListPolicies(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]ApprovalPolicy, error) {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, currency, second_level_threshold_minor,
		       created_at, updated_at, created_by, updated_by
		FROM app.approval_policies
		WHERE tenant_id = $1
		ORDER BY currency`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ApprovalPolicy, 0, 4)
	for rows.Next() {
		var (
			p        ApprovalPolicy
			currency string
		)
		if err := rows.Scan(&p.TenantID, &currency, &p.SecondLevelThresholdMinor,
			&p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &p.UpdatedBy); err != nil {
			return nil, err
		}
		p.Currency = money.Currency(currency)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) UpsertPolicy(ctx context.Context, tx pgx.Tx, p *ApprovalPolicy) (*ApprovalPolicy, error) {
	const q = `
		INSERT INTO app.approval_policies
		    (tenant_id, currency, second_level_threshold_minor, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, currency) DO UPDATE
		SET second_level_threshold_minor = EXCLUDED.second_level_threshold_minor,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = now()
		RETURNING tenant_id, currency, second_level_threshold_minor,
		          created_at, updated_at, created_by, updated_by`
	var (
		out      ApprovalPolicy
		currency string
	)
	err := tx.QueryRow(ctx, q, p.TenantID, string(p.Currency),
		p.SecondLevelThresholdMinor, p.CreatedBy, p.UpdatedBy).
		Scan(&out.TenantID, &currency, &out.SecondLevelThresholdMinor,
			&out.CreatedAt, &out.UpdatedAt, &out.CreatedBy, &out.UpdatedBy)
	if err != nil {
		return nil, err
	}
	out.Currency = money.Currency(currency)
	return &out, nil
}

func (r *Repository) DeletePolicy(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, currency string) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM app.approval_policies WHERE tenant_id = $1 AND currency = $2`,
		tenantID, currency)
	return err
}

// policyView 是审批策略的对外形态。
type policyView struct {
	Currency                  string    `json:"currency"`
	SecondLevelThresholdMinor int64     `json:"second_level_threshold_minor"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

func newPolicyView(p *ApprovalPolicy) policyView {
	return policyView{
		Currency:                  string(p.Currency),
		SecondLevelThresholdMinor: p.SecondLevelThresholdMinor,
		UpdatedAt:                 p.UpdatedAt,
	}
}

func (h *Handler) ListApprovalPolicies(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	policies, err := h.svc.ListPolicies(c.Request.Context(), actor)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]policyView, 0, len(policies))
	for i := range policies {
		items = append(items, newPolicyView(&policies[i]))
	}
	httpx.OK(c, gin.H{"items": items})
}

func (h *Handler) UpsertApprovalPolicy(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	var in PolicyInput
	if bindErr := c.ShouldBindJSON(&in); bindErr != nil {
		httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
		return
	}
	p, err := h.svc.UpsertPolicy(c.Request.Context(), actor, c.Param("currency"), in)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.OK(c, newPolicyView(p))
}

func (h *Handler) DeleteApprovalPolicy(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	if err := h.svc.DeletePolicy(c.Request.Context(), actor, c.Param("currency")); err != nil {
		httpx.Error(c, err)
		return
	}
	httpx.NoContent(c)
}
