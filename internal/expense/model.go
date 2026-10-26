// Package expense 实现费用单、预算与审批工作流。
//
// 领域规则集中在本文件：状态机、角色权限、禁止自审、可见范围。
// 数据库迁移里有一份等价的约束（状态白名单表、触发器、CHECK），
// 两层不是重复而是纵深：Go 层给出可读的 4xx，数据库层保证任何路径都写不进脏数据。
package expense

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// 业务码。前端按 code 分支，不匹配文案。
const (
	CodeStateConflict     = "expense.state_conflict"
	CodeSelfApproval      = "expense.self_approval_forbidden"
	CodeVersionConflict   = "expense.version_conflict"
	CodeBudgetNotMatched  = "expense.budget_not_matched"
	CodeBudgetUnavailable = "expense.budget_unavailable"

	CodeBudgetOverlap          = "budget.period_overlap"
	CodeBudgetInUse            = "budget.in_use"
	CodeBudgetAmountBelowSpent = "budget.amount_below_committed"
	CodeBudgetVersionConflict  = "budget.version_conflict"
)

// Status 是费用单状态。取值与数据库 CHECK 完全一致。
type Status string

const (
	StatusDraft      Status = "draft"
	StatusSubmitted  Status = "submitted"
	StatusApproved   Status = "approved"
	StatusRejected   Status = "rejected"
	StatusCancelled  Status = "cancelled"
	StatusReimbursed Status = "reimbursed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusSubmitted, StatusApproved, StatusRejected, StatusCancelled, StatusReimbursed:
		return true
	default:
		return false
	}
}

// Action 是审批动作。它与数据库 approval_steps.action 的取值一致。
type Action string

const (
	ActionSubmit    Action = "submit"
	ActionApprove   Action = "approve"
	ActionReject    Action = "reject"
	ActionCancel    Action = "cancel"
	ActionReimburse Action = "reimburse"
)

func (a Action) Valid() bool {
	_, ok := actionTransition[a]
	return ok
}

// edge 是一个动作允许的状态迁移，与 app.expense_status_transitions 的种子数据一致。
type edge struct {
	from Status
	to   Status
}

var actionTransition = map[Action]edge{
	ActionSubmit:    {from: StatusDraft, to: StatusSubmitted},
	ActionApprove:   {from: StatusSubmitted, to: StatusApproved},
	ActionReject:    {from: StatusSubmitted, to: StatusRejected},
	ActionCancel:    {from: StatusSubmitted, to: StatusCancelled},
	ActionReimburse: {from: StatusApproved, to: StatusReimbursed},
}

// CanTransition 判断是否存在 from -> to 这条边，与动作无关。
func CanTransition(from, to Status) bool {
	for _, e := range actionTransition {
		if e.from == from && e.to == to {
			return true
		}
	}
	return false
}

// Category 是费用类别，与 budgets/expenses 的 CHECK 一致。
type Category string

const (
	CategoryTravel   Category = "travel"
	CategoryMeals    Category = "meals"
	CategoryOffice   Category = "office"
	CategorySoftware Category = "software"
	CategoryOther    Category = "other"
)

func (c Category) Valid() bool {
	switch c {
	case CategoryTravel, CategoryMeals, CategoryOffice, CategorySoftware, CategoryOther:
		return true
	default:
		return false
	}
}

// ParseCategory 校验并返回类别。类别不做大小写归一化：
// 取值集合是封闭的，前端传 "Travel" 应当得到 400，而不是被悄悄映射。
func ParseCategory(raw string) (Category, error) {
	c := Category(raw)
	if !c.Valid() {
		return "", apperr.InvalidArgument("费用类别非法")
	}
	return c, nil
}

// Expense 是费用单本体。金额以最小货币单位存储，见 platform/money。
type Expense struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	SubmitterID     uuid.UUID
	Status          Status
	Category        Category
	Currency        money.Currency
	AmountMinor     int64
	Merchant        string
	Description     string
	OccurredAt      time.Time
	BudgetID        *uuid.UUID
	DecidedBy       *uuid.UUID
	DecidedAt       *time.Time
	RejectionReason string
	Version         int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedBy       uuid.UUID
	DeletedAt       *time.Time
}

func (e *Expense) IsDraft() bool { return e.Status == StatusDraft }
func (e *Expense) Deleted() bool { return e.DeletedAt != nil }

// Budget 是一个期间预算。reserved + spent <= amount 由数据库保证。
type Budget struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Name          string
	Category      Category
	Currency      money.Currency
	PeriodStart   time.Time
	PeriodEnd     time.Time
	AmountMinor   int64
	ReservedMinor int64
	SpentMinor    int64
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedBy     uuid.UUID
	DeletedAt     *time.Time
}

// AvailableMinor 是尚可用于新提交的额度。读侧展示用；
// 真正的扣减必须在 SQL 里做条件更新，不能"先查再写"。
func (b *Budget) AvailableMinor() int64 {
	return b.AmountMinor - b.ReservedMinor - b.SpentMinor
}

// ApprovalStep 是一条只追加的审批轨迹。
type ApprovalStep struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	ExpenseID  uuid.UUID
	ActorID    uuid.UUID
	Action     Action
	FromStatus Status
	ToStatus   Status
	Comment    string
	CreatedAt  time.Time
}

// Actor 是动作发起者，只能由 JWT 解析出的租户上下文构造。
type Actor struct {
	UserID uuid.UUID
	Role   tenantctx.Role
}

func (a Actor) valid() bool { return a.UserID != uuid.Nil && a.Role.Valid() }

// manage 表示管理岗：owner 与 admin。
func (a Actor) manage() bool { return a.Role.CanManageMembers() }

// ErrStateConflict 构造状态机冲突。details 里只放状态与动作，不含任何敏感信息。
func ErrStateConflict(current Status, action Action) *apperr.Error {
	return apperr.Conflict(CodeStateConflict, "费用单当前状态不允许该操作").
		WithDetail("status", string(current)).
		WithDetail("action", string(action))
}

// ErrVersionConflict 用于乐观锁失败：调用方拿着旧 version 提交修改。
func ErrVersionConflict() *apperr.Error {
	return apperr.Conflict(CodeVersionConflict, "费用单已被修改，请刷新后重试")
}

// AuthorizeAction 校验"谁能对哪张单执行哪个动作"，并返回目标状态。
//
// 两条硬规则：
//   - 审批只能由 admin/owner 执行，且任何人都不能审批自己提交的单（含 owner）；
//   - 提交只能由提交人本人执行，不允许代提交。
func AuthorizeAction(actor Actor, e *Expense, action Action) (Status, error) {
	if !actor.valid() {
		return "", apperr.Unauthorized("缺少有效的身份信息")
	}
	if !action.Valid() {
		return "", apperr.InvalidArgument("未知的审批动作")
	}
	if e.Deleted() {
		// 对已删除的资源，一律当作不存在，避免暴露"这张单曾经存在"。
		return "", apperr.NotFound("费用单")
	}
	// viewer 是只读角色：即使它曾经创建过某张草稿（例如被降级），也不能再写。
	if !actor.Role.AtLeast(tenantctx.RoleMember) {
		return "", apperr.Forbidden("只读成员不能执行该操作")
	}

	switch action {
	case ActionSubmit:
		if e.SubmitterID != actor.UserID {
			return "", apperr.Forbidden("只能提交自己的费用单")
		}
	case ActionApprove, ActionReject:
		if !actor.manage() {
			return "", apperr.Forbidden("只有管理员或所有者可以审批费用单")
		}
		if e.SubmitterID == actor.UserID {
			return "", apperr.New(CodeSelfApproval, http.StatusForbidden, "不能审批自己提交的费用单")
		}
	case ActionCancel:
		if e.SubmitterID != actor.UserID && !actor.manage() {
			return "", apperr.Forbidden("只能撤销自己的费用单")
		}
	case ActionReimburse:
		if !actor.manage() {
			return "", apperr.Forbidden("只有管理员或所有者可以标记报销")
		}
	}

	target := actionTransition[action]
	if e.Status != target.from {
		return "", ErrStateConflict(e.Status, action)
	}
	return target.to, nil
}

// AuthorizeDraftWrite 校验草稿的修改/删除权限。
// 草稿只属于提交人：管理员可以查看和审批，但不能替别人改内容或删除，
// 否则"谁填的单"这件事就失去意义。
func AuthorizeDraftWrite(actor Actor, e *Expense) error {
	if !actor.valid() {
		return apperr.Unauthorized("缺少有效的身份信息")
	}
	if e.Deleted() {
		return apperr.NotFound("费用单")
	}
	if !actor.Role.AtLeast(tenantctx.RoleMember) {
		return apperr.Forbidden("只读成员不能执行该操作")
	}
	if e.SubmitterID != actor.UserID {
		return apperr.Forbidden("只能修改或删除自己的草稿")
	}
	if !e.IsDraft() {
		return apperr.Conflict(CodeStateConflict, "只能修改或删除草稿").
			WithDetail("status", string(e.Status))
	}
	return nil
}

// CanView 决定单条费用单的可见性：admin/owner 看全部，其他角色只看自己的。
func CanView(actor Actor, e *Expense) bool {
	if !actor.valid() || e.Deleted() {
		return false
	}
	return actor.manage() || e.SubmitterID == actor.UserID
}

// Scope 是列表查询的可见范围。
type Scope string

const (
	ScopeMine Scope = "mine"
	ScopeAll  Scope = "all"
)

// ResolveScope 把查询参数收敛为最终范围。
// 非管理岗请求 all 时返回 403，而不是静默降级成 mine：
// 静默降级会让前端误以为看到的是全租户数据。
func ResolveScope(role tenantctx.Role, requested string) (Scope, error) {
	if !role.Valid() {
		return "", apperr.Unauthorized("缺少有效的身份信息")
	}
	switch Scope(requested) {
	case "", ScopeMine:
		return ScopeMine, nil
	case ScopeAll:
		if !role.CanManageMembers() {
			return "", apperr.Forbidden("无权查看全租户费用单")
		}
		return ScopeAll, nil
	default:
		return "", apperr.InvalidArgument("scope 只能是 mine 或 all")
	}
}
