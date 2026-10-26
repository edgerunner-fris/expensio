package expense

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

var (
	submitterID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	otherID     = uuid.MustParse("22222222-2222-4222-8222-222222222222")
)

func actor(role tenantctx.Role) Actor { return Actor{UserID: submitterID, Role: role} }

func expense(status Status) *Expense {
	return &Expense{ID: uuid.New(), SubmitterID: submitterID, Status: status, Version: 1}
}

func requireAppErr(t *testing.T, err error, code string, status int) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, code, apperr.Code(err), "业务码")
	assert.Equal(t, status, apperr.Status(err), "HTTP 状态码")
}

func TestStatusValid(t *testing.T) {
	t.Parallel()

	for _, s := range []Status{StatusDraft, StatusSubmitted, StatusApproved, StatusRejected, StatusCancelled, StatusReimbursed} {
		assert.True(t, s.Valid(), "%s 必须是合法状态", s)
	}
	for _, s := range []Status{"", "DRAFT", "paid", "deleted"} {
		assert.False(t, s.Valid(), "%q 不是合法状态", s)
	}
}

// TestTransitionsMatchDatabaseSeed 用 6x6 全矩阵锁定状态机，
// 防止后续改动悄悄多出或删掉一条边。
func TestTransitionsMatchDatabaseSeed(t *testing.T) {
	t.Parallel()

	all := []Status{StatusDraft, StatusSubmitted, StatusApproved, StatusRejected, StatusCancelled, StatusReimbursed}
	allowed := map[[2]Status]bool{
		{StatusDraft, StatusSubmitted}:     true,
		{StatusSubmitted, StatusApproved}:  true,
		{StatusSubmitted, StatusRejected}:  true,
		{StatusSubmitted, StatusCancelled}: true,
		{StatusApproved, StatusReimbursed}: true,
	}
	edges := 0
	for _, from := range all {
		for _, to := range all {
			assert.Equal(t, allowed[[2]Status{from, to}], CanTransition(from, to),
				"%s -> %s", from, to)
		}
	}
	for range actionTransition {
		edges++
	}
	assert.Equal(t, 5, edges, "状态边数量必须与迁移中的种子数据一致")
}

func TestActionValid(t *testing.T) {
	t.Parallel()

	for _, a := range []Action{ActionSubmit, ActionApprove, ActionReject, ActionCancel, ActionReimburse} {
		assert.True(t, a.Valid(), "%s 必须是合法动作", a)
	}
	for _, a := range []Action{"", "pay", "delete"} {
		assert.False(t, a.Valid(), "%q 不是合法动作", a)
	}
}

func TestParseCategory(t *testing.T) {
	t.Parallel()

	for _, c := range []string{"travel", "meals", "office", "software", "other"} {
		got, err := ParseCategory(c)
		require.NoError(t, err)
		assert.Equal(t, Category(c), got)
	}
	for _, c := range []string{"", "Travel", "fuel", "travel "} {
		_, err := ParseCategory(c)
		requireAppErr(t, err, apperr.CodeInvalidArgument, 400)
	}
}

func TestAuthorizeAction(t *testing.T) {
	t.Parallel()

	other := func(role tenantctx.Role) Actor { return Actor{UserID: otherID, Role: role} }

	cases := []struct {
		name     string
		actor    Actor
		expense  *Expense
		action   Action
		wantNext Status
		wantCode string
		wantHTTP int
	}{
		{
			name: "成员提交自己的草稿", actor: actor(tenantctx.RoleMember),
			expense: expense(StatusDraft), action: ActionSubmit,
			wantNext: StatusSubmitted,
		},
		{
			name: "管理员也不能代提交", actor: other(tenantctx.RoleAdmin),
			expense: expense(StatusDraft), action: ActionSubmit,
			wantCode: apperr.CodeForbidden, wantHTTP: 403,
		},
		{
			name: "viewer 不能提交自己的草稿", actor: actor(tenantctx.RoleViewer),
			expense: expense(StatusDraft), action: ActionSubmit,
			wantCode: apperr.CodeForbidden, wantHTTP: 403,
		},
		{
			name: "管理员审批他人提交单", actor: other(tenantctx.RoleAdmin),
			expense: expense(StatusSubmitted), action: ActionApprove,
			wantNext: StatusApproved,
		},
		{
			name: "所有者不能审批自己的单", actor: actor(tenantctx.RoleOwner),
			expense: expense(StatusSubmitted), action: ActionApprove,
			wantCode: CodeSelfApproval, wantHTTP: 403,
		},
		{
			name: "管理员不能驳回自己的单", actor: actor(tenantctx.RoleAdmin),
			expense: expense(StatusSubmitted), action: ActionReject,
			wantCode: CodeSelfApproval, wantHTTP: 403,
		},
		{
			name: "成员不能审批", actor: other(tenantctx.RoleMember),
			expense: expense(StatusSubmitted), action: ActionApprove,
			wantCode: apperr.CodeForbidden, wantHTTP: 403,
		},
		{
			name: "审批草稿是状态冲突", actor: other(tenantctx.RoleAdmin),
			expense: expense(StatusDraft), action: ActionApprove,
			wantCode: CodeStateConflict, wantHTTP: 409,
		},
		{
			name: "成员撤销自己的提交单", actor: actor(tenantctx.RoleMember),
			expense: expense(StatusSubmitted), action: ActionCancel,
			wantNext: StatusCancelled,
		},
		{
			name: "管理员撤销他人的提交单", actor: other(tenantctx.RoleAdmin),
			expense: expense(StatusSubmitted), action: ActionCancel,
			wantNext: StatusCancelled,
		},
		{
			name: "成员不能撤销他人的单", actor: other(tenantctx.RoleMember),
			expense: expense(StatusSubmitted), action: ActionCancel,
			wantCode: apperr.CodeForbidden, wantHTTP: 403,
		},
		{
			name: "标记报销", actor: other(tenantctx.RoleAdmin),
			expense: expense(StatusApproved), action: ActionReimburse,
			wantNext: StatusReimbursed,
		},
		{
			name: "成员不能标记报销", actor: actor(tenantctx.RoleMember),
			expense: expense(StatusApproved), action: ActionReimburse,
			wantCode: apperr.CodeForbidden, wantHTTP: 403,
		},
		{
			name: "已撤销的单不能重新提交", actor: actor(tenantctx.RoleMember),
			expense: expense(StatusCancelled), action: ActionSubmit,
			wantCode: CodeStateConflict, wantHTTP: 409,
		},
		{
			name: "已删除的单当作不存在", actor: actor(tenantctx.RoleMember),
			expense: func() *Expense {
				e := expense(StatusDraft)
				now := e.CreatedAt
				e.DeletedAt = &now
				return e
			}(), action: ActionSubmit,
			wantCode: apperr.CodeNotFound, wantHTTP: 404,
		},
		{
			name: "未知动作", actor: actor(tenantctx.RoleOwner),
			expense: expense(StatusDraft), action: "delete",
			wantCode: apperr.CodeInvalidArgument, wantHTTP: 400,
		},
		{
			name: "缺少身份", actor: Actor{Role: tenantctx.RoleOwner},
			expense: expense(StatusDraft), action: ActionSubmit,
			wantCode: apperr.CodeUnauthorized, wantHTTP: 401,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next, err := AuthorizeAction(tc.actor, tc.expense, tc.action)
			if tc.wantCode == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.wantNext, next)
				return
			}
			requireAppErr(t, err, tc.wantCode, tc.wantHTTP)
			assert.Empty(t, next, "失败时不应返回目标状态")
		})
	}
}

func TestAuthorizeActionStateConflictCarriesDetails(t *testing.T) {
	t.Parallel()

	_, err := AuthorizeAction(actor(tenantctx.RoleMember), expense(StatusApproved), ActionSubmit)
	requireAppErr(t, err, CodeStateConflict, 409)
	assert.Equal(t, map[string]any{"status": "approved", "action": "submit"}, apperr.Details(err))
}

func TestAuthorizeDraftWrite(t *testing.T) {
	t.Parallel()

	other := Actor{UserID: otherID, Role: tenantctx.RoleAdmin}

	require.NoError(t, AuthorizeDraftWrite(actor(tenantctx.RoleMember), expense(StatusDraft)))

	requireAppErr(t, AuthorizeDraftWrite(other, expense(StatusDraft)),
		apperr.CodeForbidden, 403)
	requireAppErr(t, AuthorizeDraftWrite(actor(tenantctx.RoleMember), expense(StatusSubmitted)),
		CodeStateConflict, 409)
	requireAppErr(t, AuthorizeDraftWrite(actor(tenantctx.RoleViewer), expense(StatusDraft)),
		apperr.CodeForbidden, 403)
	requireAppErr(t, AuthorizeDraftWrite(Actor{Role: tenantctx.RoleMember}, expense(StatusDraft)),
		apperr.CodeUnauthorized, 401)
}

func TestCanView(t *testing.T) {
	t.Parallel()

	e := expense(StatusSubmitted)
	assert.True(t, CanView(actor(tenantctx.RoleMember), e), "成员可以看自己的单")
	assert.True(t, CanView(actor(tenantctx.RoleViewer), e), "viewer 可以看自己的单")
	assert.True(t, CanView(Actor{UserID: otherID, Role: tenantctx.RoleAdmin}, e))
	assert.True(t, CanView(Actor{UserID: otherID, Role: tenantctx.RoleOwner}, e))
	assert.False(t, CanView(Actor{UserID: otherID, Role: tenantctx.RoleMember}, e))
	assert.False(t, CanView(Actor{UserID: otherID, Role: tenantctx.RoleViewer}, e))

	now := e.UpdatedAt
	e.DeletedAt = &now
	assert.False(t, CanView(actor(tenantctx.RoleOwner), e), "已删除的单不可见")
}

func TestResolveScope(t *testing.T) {
	t.Parallel()

	got, err := ResolveScope(tenantctx.RoleMember, "")
	require.NoError(t, err)
	assert.Equal(t, ScopeMine, got, "默认只看自己的")

	got, err = ResolveScope(tenantctx.RoleViewer, "mine")
	require.NoError(t, err)
	assert.Equal(t, ScopeMine, got)

	got, err = ResolveScope(tenantctx.RoleAdmin, "all")
	require.NoError(t, err)
	assert.Equal(t, ScopeAll, got)

	got, err = ResolveScope(tenantctx.RoleOwner, "all")
	require.NoError(t, err)
	assert.Equal(t, ScopeAll, got)

	_, err = ResolveScope(tenantctx.RoleMember, "all")
	requireAppErr(t, err, apperr.CodeForbidden, 403)

	_, err = ResolveScope(tenantctx.RoleOwner, "ALL")
	requireAppErr(t, err, apperr.CodeInvalidArgument, 400)

	_, err = ResolveScope("root", "mine")
	requireAppErr(t, err, apperr.CodeUnauthorized, 401)
}

func TestBudgetAvailableMinor(t *testing.T) {
	t.Parallel()

	b := &Budget{AmountMinor: 100_000, ReservedMinor: 30_000, SpentMinor: 20_000}
	assert.EqualValues(t, 50_000, b.AvailableMinor())
}
