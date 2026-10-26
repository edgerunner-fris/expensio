package expense

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/money"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func newBudgetInput() BudgetInput {
	return BudgetInput{
		Name:        "2026-10 travel",
		Category:    "travel",
		Currency:    "USD",
		PeriodStart: "2026-10-01",
		PeriodEnd:   "2026-10-31",
		AmountMinor: 100_000,
	}
}

func updateBudgetInput(b *Budget) BudgetUpdateInput {
	return BudgetUpdateInput{
		Version:     b.Version,
		Name:        b.Name,
		PeriodStart: b.PeriodStart.Format(dateLayout),
		PeriodEnd:   b.PeriodEnd.Format(dateLayout),
		AmountMinor: b.AmountMinor,
	}
}

func TestBudgetCRUDAndRoles(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant := uuid.New()
	admin, member := uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho := Actor{UserID: admin, Role: tenantctx.RoleAdmin}
	memberWho := Actor{UserID: member, Role: tenantctx.RoleMember}

	_, err := svc.CreateBudget(memberCtx, memberWho, newBudgetInput())
	requireAppErr(t, err, apperr.CodeForbidden, 403)

	b, err := svc.CreateBudget(adminCtx, adminWho, newBudgetInput())
	require.NoError(t, err)
	assert.Equal(t, tenant, b.TenantID)
	assert.Equal(t, 1, b.Version)
	assert.Equal(t, int64(100_000), b.AmountMinor)
	assert.Zero(t, b.ReservedMinor)
	assert.Zero(t, b.SpentMinor)

	// 成员与 viewer 都可以读预算（提交时需要选择）。
	got, err := svc.GetBudget(memberCtx, memberWho, b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.ID)
	viewerWho := Actor{UserID: uuid.New(), Role: tenantctx.RoleViewer}
	_, err = svc.GetBudget(tenantCtx(t, tenant, viewerWho.UserID, tenantctx.RoleViewer), viewerWho, b.ID)
	require.NoError(t, err)

	in := updateBudgetInput(b)
	in.Name = "renamed"
	in.AmountMinor = 200_000
	updated, err := svc.UpdateBudget(adminCtx, adminWho, b.ID, in)
	require.NoError(t, err)
	assert.Equal(t, 2, updated.Version)
	assert.Equal(t, "renamed", updated.Name)
	assert.Equal(t, int64(200_000), updated.AmountMinor)

	// 旧版本再次提交：409。
	in.Version = 1
	_, err = svc.UpdateBudget(adminCtx, adminWho, b.ID, in)
	requireAppErr(t, err, CodeBudgetVersionConflict, 409)

	items, total, err := svc.ListBudgets(memberCtx, memberWho, BudgetQuery{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, items, 1)

	require.NoError(t, svc.DeleteBudget(adminCtx, adminWho, b.ID, 2))
	_, err = svc.GetBudget(adminCtx, adminWho, b.ID)
	requireAppErr(t, err, apperr.CodeNotFound, 404)

	_, total, err = svc.ListBudgets(adminCtx, adminWho, BudgetQuery{})
	require.NoError(t, err)
	assert.Zero(t, total)
}

func TestBudgetOverlapAndDeleteThenRecreate(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	who := Actor{UserID: admin, Role: tenantctx.RoleAdmin}

	base, err := svc.CreateBudget(ctx, who, newBudgetInput())
	require.NoError(t, err)

	overlap := newBudgetInput()
	overlap.PeriodStart, overlap.PeriodEnd = "2026-10-15", "2026-11-15"
	_, err = svc.CreateBudget(ctx, who, overlap)
	requireAppErr(t, err, CodeBudgetOverlap, 409)

	adjacent := newBudgetInput()
	adjacent.PeriodStart, adjacent.PeriodEnd = "2026-11-01", "2026-11-30"
	_, err = svc.CreateBudget(ctx, who, adjacent)
	require.NoError(t, err, "相邻期间不重叠")

	otherCategory := newBudgetInput()
	otherCategory.Category = "meals"
	_, err = svc.CreateBudget(ctx, who, otherCategory)
	require.NoError(t, err, "不同类别允许同期间")

	otherCurrency := newBudgetInput()
	otherCurrency.Currency = "EUR"
	_, err = svc.CreateBudget(ctx, who, otherCurrency)
	require.NoError(t, err, "不同币种允许同期间")

	// 软删之后同一期间可以重建；version 从 1 重新开始。
	require.NoError(t, svc.DeleteBudget(ctx, who, base.ID, 1))
	recreated, err := svc.CreateBudget(ctx, who, newBudgetInput())
	require.NoError(t, err)
	assert.NotEqual(t, base.ID, recreated.ID)
}

func TestBudgetCommittedMoneyProtection(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	who := Actor{UserID: admin, Role: tenantctx.RoleAdmin}

	b, err := svc.CreateBudget(ctx, who, newBudgetInput())
	require.NoError(t, err)

	// 占用 60000：触发器会让 version 前进到 2。
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET reserved_minor = 60000 WHERE id = $1`, b.ID))
	b, err = svc.GetBudget(ctx, who, b.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, b.Version, "任何 UPDATE 都必须让 version 前进")

	in := updateBudgetInput(b)
	in.AmountMinor = 50_000
	_, err = svc.UpdateBudget(ctx, who, b.ID, in)
	requireAppErr(t, err, CodeBudgetAmountBelowSpent, 409)

	in.AmountMinor = 60_000 // 恰好等于已占用，允许
	b, err = svc.UpdateBudget(ctx, who, b.ID, in)
	require.NoError(t, err)

	err = svc.DeleteBudget(ctx, who, b.ID, b.Version)
	requireAppErr(t, err, CodeBudgetInUse, 409)

	// 清空占用后可删除。
	require.NoError(t, execInTenant(t, database, ctx,
		`UPDATE app.budgets SET reserved_minor = 0 WHERE id = $1`, b.ID))
	b, err = svc.GetBudget(ctx, who, b.ID)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteBudget(ctx, who, b.ID, b.Version))
}

func TestBudgetPeriodChangeBlockedWhenReferenced(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenant, admin, member := uuid.New(), uuid.New(), uuid.New()
	adminCtx := tenantCtx(t, tenant, admin, tenantctx.RoleAdmin)
	memberCtx := tenantCtx(t, tenant, member, tenantctx.RoleMember)
	adminWho := Actor{UserID: admin, Role: tenantctx.RoleAdmin}
	memberWho := Actor{UserID: member, Role: tenantctx.RoleMember}

	b, err := svc.CreateBudget(adminCtx, adminWho, newBudgetInput())
	require.NoError(t, err)

	in := validInput()
	in.BudgetID = &b.ID
	_, err = svc.CreateDraft(memberCtx, memberWho, in)
	require.NoError(t, err)

	change := updateBudgetInput(b)
	change.PeriodStart, change.PeriodEnd = "2026-11-01", "2026-11-30"
	_, err = svc.UpdateBudget(adminCtx, adminWho, b.ID, change)
	requireAppErr(t, err, CodeBudgetInUse, 409)

	// 不改期间时仍然可以调整金额与名称。
	sameAmount := updateBudgetInput(b)
	sameAmount.Name = "still october"
	updated, err := svc.UpdateBudget(adminCtx, adminWho, b.ID, sameAmount)
	require.NoError(t, err)
	assert.Equal(t, "still october", updated.Name)
}

func resolveBudgetFor(t *testing.T, svc *Service, database *db.DB, ctx context.Context, e *Expense) (*uuid.UUID, error) {
	t.Helper()
	var out *uuid.UUID
	err := database.WithinTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		id, err := svc.resolveBudget(ctx, tx, e)
		out = id
		return err
	})
	return out, err
}

func TestResolveBudget(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)
	tenantA, adminA := uuid.New(), uuid.New()
	tenantB, adminB := uuid.New(), uuid.New()
	ctxA := tenantCtx(t, tenantA, adminA, tenantctx.RoleAdmin)
	ctxB := tenantCtx(t, tenantB, adminB, tenantctx.RoleAdmin)

	// 租户从未配置预算：允许不带预算。
	id, err := resolveBudgetFor(t, svc, database, ctxB, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"), OccurredAt: testClock,
	})
	require.NoError(t, err)
	assert.Nil(t, id)

	travelA, err := svc.CreateBudget(ctxA, Actor{UserID: adminA, Role: tenantctx.RoleAdmin}, newBudgetInput())
	require.NoError(t, err)
	meals := newBudgetInput()
	meals.Category = "meals"
	_, err = svc.CreateBudget(ctxA, Actor{UserID: adminA, Role: tenantctx.RoleAdmin}, meals)
	require.NoError(t, err)

	// 自动匹配。
	id, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"), OccurredAt: testClock,
	})
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, travelA.ID, *id)

	// 显式指定且匹配。
	explicit := travelA.ID
	id, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"),
		OccurredAt: testClock, BudgetID: &explicit,
	})
	require.NoError(t, err)
	require.NotNil(t, id)
	assert.Equal(t, travelA.ID, *id)

	// 指定了类别不匹配的预算。
	mealsList, _, err := svc.ListBudgets(ctxA, Actor{UserID: adminA, Role: tenantctx.RoleAdmin}, BudgetQuery{Category: "meals"})
	require.NoError(t, err)
	require.Len(t, mealsList, 1)
	mealsID := mealsList[0].ID
	_, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"),
		OccurredAt: testClock, BudgetID: &mealsID,
	})
	requireAppErr(t, err, CodeBudgetNotMatched, 409)

	// 有预算但日期不在任何期间内。
	_, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"),
		OccurredAt: testClock.AddDate(0, 2, 0),
	})
	requireAppErr(t, err, CodeBudgetNotMatched, 409)

	// 指定不存在的预算。
	missing := uuid.New()
	_, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"),
		OccurredAt: testClock, BudgetID: &missing,
	})
	requireAppErr(t, err, CodeBudgetNotMatched, 409)

	// 指定别的租户的预算：RLS 下等同于不存在。
	foreign, err := svc.CreateBudget(ctxB, Actor{UserID: adminB, Role: tenantctx.RoleAdmin}, newBudgetInput())
	require.NoError(t, err)
	_, err = resolveBudgetFor(t, svc, database, ctxA, &Expense{
		Category: CategoryTravel, Currency: money.Currency("USD"),
		OccurredAt: testClock, BudgetID: &foreign.ID,
	})
	requireAppErr(t, err, CodeBudgetNotMatched, 409)
}
