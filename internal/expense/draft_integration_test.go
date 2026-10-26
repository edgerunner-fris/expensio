package expense

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func newService(t *testing.T, database *db.DB) *Service {
	t.Helper()
	s := NewService(database)
	// 固定时钟：否则"发生时间不能在未来"会随真实时间漂移。
	s.now = func() time.Time { return testClock }
	return s
}

func validInput() DraftInput {
	return DraftInput{
		Category:    "travel",
		Currency:    "USD",
		AmountMinor: 12345,
		Merchant:    "  Acme Air  ",
		Description: "机票",
		OccurredAt:  testClock.Add(-24 * time.Hour),
	}
}

func TestCreateAndGetDraft(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)

	budget := insertBudget(t, database, ctx, "travel", day("2026-10-01"), day("2026-10-31"))
	in := validInput()
	in.Currency = "usd" // 小写应被归一化
	in.BudgetID = &budget

	e, err := svc.CreateDraft(ctx, Actor{UserID: user, Role: tenantctx.RoleMember}, in)
	require.NoError(t, err)
	assert.Equal(t, tenant, e.TenantID, "tenant_id 只能来自上下文")
	assert.Equal(t, user, e.SubmitterID, "提交人只能是当前用户")
	assert.Equal(t, StatusDraft, e.Status)
	assert.Equal(t, 1, e.Version)
	assert.Equal(t, money.Currency("USD"), e.Currency)
	assert.Equal(t, "Acme Air", e.Merchant, "首尾空白应被裁掉")
	assert.NotNil(t, e.BudgetID)
	assert.False(t, e.CreatedAt.IsZero())

	got, err := svc.Get(ctx, Actor{UserID: user, Role: tenantctx.RoleMember}, e.ID)
	require.NoError(t, err)
	assert.Equal(t, e.ID, got.ID)
	assert.Equal(t, int64(12345), got.AmountMinor)
	assert.WithinDuration(t, e.CreatedAt, got.CreatedAt, time.Second)
}

func TestDraftPermission(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant := uuid.New()
	author := uuid.New()
	other := uuid.New()
	ctx := tenantCtx(t, tenant, author, tenantctx.RoleMember)

	_, err := svc.CreateDraft(ctx, Actor{UserID: other, Role: tenantctx.RoleViewer}, validInput())
	requireAppErr(t, err, apperr.CodeForbidden, 403)

	_, err = svc.CreateDraft(ctx, Actor{Role: tenantctx.RoleMember}, validInput())
	requireAppErr(t, err, apperr.CodeUnauthorized, 401)

	e, err := svc.CreateDraft(ctx, Actor{UserID: author, Role: tenantctx.RoleMember}, validInput())
	require.NoError(t, err)

	// 同租户的普通成员看不到别人的单：404 而不是 403，避免 ID 探测。
	_, err = svc.Get(ctx, Actor{UserID: other, Role: tenantctx.RoleMember}, e.ID)
	requireAppErr(t, err, apperr.CodeNotFound, 404)

	// 管理员可以查看。
	_, err = svc.Get(ctx, Actor{UserID: other, Role: tenantctx.RoleAdmin}, e.ID)
	require.NoError(t, err)

	// 但管理员不能替别人改草稿或删草稿。
	_, err = svc.UpdateDraft(ctx, Actor{UserID: other, Role: tenantctx.RoleAdmin}, e.ID, 1, validInput())
	requireAppErr(t, err, apperr.CodeForbidden, 403)
	err = svc.DeleteDraft(ctx, Actor{UserID: other, Role: tenantctx.RoleAdmin}, e.ID, 1)
	requireAppErr(t, err, apperr.CodeForbidden, 403)

	// 别的租户连 404 的前提条件都不同：RLS 下直接查不到。
	foreignCtx := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleOwner)
	_, err = svc.Get(foreignCtx, Actor{UserID: author, Role: tenantctx.RoleOwner}, e.ID)
	requireAppErr(t, err, apperr.CodeNotFound, 404)
}

func TestUpdateDraftOptimisticLock(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	who := Actor{UserID: user, Role: tenantctx.RoleMember}

	e, err := svc.CreateDraft(ctx, who, validInput())
	require.NoError(t, err)

	in := validInput()
	in.Merchant = "Second"
	updated, err := svc.UpdateDraft(ctx, who, e.ID, 1, in)
	require.NoError(t, err)
	assert.Equal(t, 2, updated.Version)
	assert.Equal(t, "Second", updated.Merchant)

	// 拿着旧版本再改：必须 409，而不是覆盖别人的修改。
	in.Merchant = "Stale"
	_, err = svc.UpdateDraft(ctx, who, e.ID, 1, in)
	requireAppErr(t, err, CodeVersionConflict, 409)

	// 提交后的单据不可再编辑。
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.expenses SET status = 'submitted' WHERE id = $1`, e.ID))
	_, err = svc.UpdateDraft(ctx, who, e.ID, 2, in)
	requireAppErr(t, err, CodeStateConflict, 409)

	_, err = svc.UpdateDraft(ctx, who, uuid.New(), 1, in)
	requireAppErr(t, err, apperr.CodeNotFound, 404)

	_, err = svc.UpdateDraft(ctx, who, e.ID, 0, in)
	requireAppErr(t, err, apperr.CodeInvalidArgument, 400)
}

func TestDeleteDraft(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	who := Actor{UserID: user, Role: tenantctx.RoleMember}

	e, err := svc.CreateDraft(ctx, who, validInput())
	require.NoError(t, err)

	// 版本不对：409。
	err = svc.DeleteDraft(ctx, who, e.ID, 99)
	requireAppErr(t, err, CodeVersionConflict, 409)

	require.NoError(t, svc.DeleteDraft(ctx, who, e.ID, 1))
	_, err = svc.Get(ctx, who, e.ID)
	requireAppErr(t, err, apperr.CodeNotFound, 404)

	// 软删而不是物理删除：行还在，deleted_at 已写入。
	assert.Equal(t, 1, countRows(t, database, ctx,
		`SELECT count(*) FROM app.expenses WHERE id = $1 AND deleted_at IS NOT NULL`, e.ID))
	assert.Equal(t, 0, countRows(t, database, ctx,
		`SELECT count(*) FROM app.expenses WHERE id = $1 AND deleted_at IS NULL`, e.ID))

	// 再删一次：已经不存在。
	err = svc.DeleteDraft(ctx, who, e.ID, 1)
	requireAppErr(t, err, apperr.CodeNotFound, 404)
}

func TestListDrafts(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant := uuid.New()
	alice, bob, admin := uuid.New(), uuid.New(), uuid.New()
	aliceCtx := tenantCtx(t, tenant, alice, tenantctx.RoleMember)
	bobCtx := tenantCtx(t, tenant, bob, tenantctx.RoleMember)
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)

	aliceWho := Actor{UserID: alice, Role: tenantctx.RoleMember}
	bobWho := Actor{UserID: bob, Role: tenantctx.RoleMember}
	adminWho := Actor{UserID: admin, Role: tenantctx.RoleAdmin}

	for i := 0; i < 2; i++ {
		_, err := svc.CreateDraft(aliceCtx, aliceWho, validInput())
		require.NoError(t, err)
	}
	if _, err := svc.CreateDraft(bobCtx, bobWho, validInput()); err != nil {
		t.Fatal(err)
	}

	items, total, err := svc.List(aliceCtx, aliceWho, ListQuery{})
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	assert.Len(t, items, 2)

	items, total, err = svc.List(adminCtx, adminWho, ListQuery{Scope: "all"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	assert.Len(t, items, 3)

	// 非管理岗请求全租户：403，不能静默降级。
	_, _, err = svc.List(aliceCtx, aliceWho, ListQuery{Scope: "all"})
	requireAppErr(t, err, apperr.CodeForbidden, 403)

	// 状态过滤。
	_, total, err = svc.List(adminCtx, adminWho, ListQuery{Scope: "all", Status: "submitted"})
	require.NoError(t, err)
	assert.Zero(t, total)

	_, _, err = svc.List(adminCtx, adminWho, ListQuery{Scope: "all", Status: "flying"})
	requireAppErr(t, err, apperr.CodeInvalidArgument, 400)

	// 分页：两页各一条。
	page1, total, err := svc.List(aliceCtx, aliceWho, ListQuery{Limit: 1, Offset: 0})
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	assert.Len(t, page1, 1)
	page2, _, err := svc.List(aliceCtx, aliceWho, ListQuery{Limit: 1, Offset: 1})
	require.NoError(t, err)
	assert.Len(t, page2, 1)
	assert.NotEqual(t, page1[0].ID, page2[0].ID)

	// 另一个租户看不到任何数据。
	otherCtx := tenantCtx(t, uuid.New(), uuid.New(), tenantctx.RoleAdmin)
	items, total, err = svc.List(otherCtx, Actor{UserID: uuid.New(), Role: tenantctx.RoleAdmin},
		ListQuery{Scope: "all"})
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, items)
}

func TestDraftValidation(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, user := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, user, tenantctx.RoleMember)
	who := Actor{UserID: user, Role: tenantctx.RoleMember}

	cases := []struct {
		name string
		mod  func(*DraftInput)
	}{
		{"金额为零", func(in *DraftInput) { in.AmountMinor = 0 }},
		{"金额为负", func(in *DraftInput) { in.AmountMinor = -1 }},
		{"金额超上限", func(in *DraftInput) { in.AmountMinor = 1_000_000_000_001 }},
		{"币种非法", func(in *DraftInput) { in.Currency = "US" }},
		{"类别非法", func(in *DraftInput) { in.Category = "Travel" }},
		{"发生时间为零值", func(in *DraftInput) { in.OccurredAt = time.Time{} }},
		{"发生时间在未来", func(in *DraftInput) { in.OccurredAt = testClock.Add(time.Hour) }},
		{"商家超长", func(in *DraftInput) { in.Merchant = strings.Repeat("商", 201) }},
		{"说明超长", func(in *DraftInput) { in.Description = strings.Repeat("x", 2001) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mod(&in)
			_, err := svc.CreateDraft(ctx, who, in)
			requireAppErr(t, err, apperr.CodeInvalidArgument, 400)
		})
	}

	// 边界值应当通过：200 个汉字是 600 字节，但数据库按字符计数。
	in := validInput()
	in.Merchant = strings.Repeat("商", 200)
	in.Description = strings.Repeat("x", 2000)
	in.OccurredAt = testClock.Add(maxOccurredSkew)
	_, err := svc.CreateDraft(ctx, who, in)
	require.NoError(t, err, "恰好等于上限的输入必须被接受")
}

func TestDraftBudgetMustBelongToTenant(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenantA, userA := uuid.New(), uuid.New()
	tenantB, userB := uuid.New(), uuid.New()
	ctxA := tenantCtx(t, tenantA, userA, tenantctx.RoleOwner)
	ctxB := tenantCtx(t, tenantB, userB, tenantctx.RoleOwner)
	who := Actor{UserID: userA, Role: tenantctx.RoleOwner}

	foreignBudget := insertBudget(t, database, ctxB, "travel", day("2026-10-01"), day("2026-10-31"))
	in := validInput()
	in.BudgetID = &foreignBudget
	_, err := svc.CreateDraft(ctxA, who, in)
	requireAppErr(t, err, apperr.CodeInvalidArgument, 400)

	localBudget := insertBudget(t, database, ctxA, "travel", day("2026-10-01"), day("2026-10-31"))
	in.BudgetID = &localBudget
	e, err := svc.CreateDraft(ctxA, who, in)
	require.NoError(t, err)
	require.NotNil(t, e.BudgetID)
	assert.Equal(t, localBudget, *e.BudgetID)

	// 草稿阶段可以清空预算，提交时才强制匹配。
	in.BudgetID = nil
	in.Merchant = "no budget"
	updated, err := svc.UpdateDraft(ctxA, who, e.ID, 1, in)
	require.NoError(t, err)
	assert.Nil(t, updated.BudgetID)
}
