package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/expense"
	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/events"
	"github.com/edgerunner-fris/expensio/internal/platform/kafka"
	"github.com/edgerunner-fris/expensio/internal/platform/outbox"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
	"github.com/edgerunner-fris/expensio/internal/report"
)

const (
	e2eJWTSecret    = "e2e-report-secret-at-least-32-bytes!"
	e2eConsumerName = "report-e2e"
	e2eGroupID      = "report-e2e-group"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

func openDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	database, err := db.Open(context.Background(), dsn, 5)
	require.NoError(t, err)
	t.Cleanup(database.Close)
	return database
}

func withTenant(t *testing.T, tenantID, userID uuid.UUID, role tenantctx.Role) context.Context {
	t.Helper()
	ctx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID:     tenantID,
		Slug:   "e2e-" + tenantID.String()[:8],
		UserID: userID,
		Role:   role,
	})
	require.NoError(t, err)
	return ctx
}

// eventually 轮询直到条件成立。跨服务的链路是异步的，
// 断言必须等待事实落库，而不是假定投递与消费已经完成。
func eventually(t *testing.T, timeout time.Duration, condition func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf(msg, args...)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func factState(t *testing.T, database *db.DB, tenantID, expenseID uuid.UUID) (status string, version int, found bool) {
	t.Helper()
	err := database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT status, version
				FROM app.expense_facts
				WHERE tenant_id = $1 AND expense_id = $2`,
				tenantID, expenseID).Scan(&status, &version)
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false
	}
	require.NoError(t, err)
	return status, version, true
}

func processedCount(t *testing.T, database *db.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithinTenantID(context.Background(), tenantID,
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*)
				FROM app.processed_events
				WHERE tenant_id = $1 AND consumer = $2`,
				tenantID, e2eConsumerName).Scan(&n)
		}))
	return n
}

func unpublishedOutbox(t *testing.T, database *db.DB, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM app.outbox_events
			WHERE tenant_id = $1 AND published_at IS NULL`, tenantID).Scan(&n)
	}))
	return n
}

func fetchSummary(t *testing.T, engine *gin.Engine, token, path string) report.Summary {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var env struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "ok", env.Code, rec.Body.String())

	var out report.Summary
	require.NoError(t, json.Unmarshal(env.Data, &out))
	return out
}

// TestExpenseEventsReachReportSummary 走完整链路：
// 费用服务写业务数据与 outbox → 投递器发到真实 Kafka → report 消费者
// 写入读模型 → 报表 HTTP API 返回聚合结果。
func TestExpenseEventsReachReportSummary(t *testing.T) {
	pg := testenv.StartPostgres(t, "expense", "report")
	expenseDB := openDB(t, pg.AppDSN("expense"))
	reportDB := openDB(t, pg.AppDSN("report"))

	k := testenv.StartKafka(t)
	k.CreateTopic(t, events.TopicExpense, 6)
	k.CreateTopic(t, events.TopicExpenseDLQ, 6)

	producer, err := kafka.NewProducer(k.Brokers, "expense-e2e")
	require.NoError(t, err)
	t.Cleanup(producer.Close)

	consumer, err := report.NewConsumer(report.ConsumerOptions{
		Database:     reportDB,
		Brokers:      k.Brokers,
		Publisher:    producer,
		Logger:       zap.NewNop(),
		ClientID:     "report-e2e",
		GroupID:      e2eGroupID,
		ConsumerName: e2eConsumerName,
		RetryBase:    10 * time.Millisecond,
	})
	require.NoError(t, err)

	runCtx, stopConsumer := context.WithCancel(context.Background())
	consumerDone := make(chan error, 1)
	go func() { consumerDone <- consumer.Run(runCtx) }()
	// 先停消费者再关数据库/生产者：Cleanup 是后进先出，
	// 因此这个清理必须注册在数据库与生产者之后。
	t.Cleanup(func() {
		stopConsumer()
		select {
		case <-consumerDone:
		case <-time.After(15 * time.Second):
			t.Error("report 消费者未在超时内退出")
		}
		consumer.Close()
	})

	tenantID := uuid.New()
	adminID, memberID := uuid.New(), uuid.New()
	adminCtx := withTenant(t, tenantID, adminID, tenantctx.RoleAdmin)
	memberCtx := withTenant(t, tenantID, memberID, tenantctx.RoleMember)
	admin := expense.Actor{UserID: adminID, Role: tenantctx.RoleAdmin}
	member := expense.Actor{UserID: memberID, Role: tenantctx.RoleMember}

	svc := expense.NewService(expenseDB)
	occurredAt := time.Now().UTC().Add(-2 * time.Hour)
	day := occurredAt.Format("2006-01-02")
	periodStart := occurredAt.AddDate(0, 0, -10).Format("2006-01-02")
	periodEnd := occurredAt.AddDate(0, 0, 10).Format("2006-01-02")

	travelBudget, err := svc.CreateBudget(adminCtx, admin, expense.BudgetInput{
		Name: "差旅", Category: "travel", Currency: "USD",
		PeriodStart: periodStart, PeriodEnd: periodEnd, AmountMinor: 500_000,
	})
	require.NoError(t, err)
	mealsBudget, err := svc.CreateBudget(adminCtx, admin, expense.BudgetInput{
		Name: "餐饮", Category: "meals", Currency: "EUR",
		PeriodStart: periodStart, PeriodEnd: periodEnd, AmountMinor: 200_000,
	})
	require.NoError(t, err)

	travel, err := svc.CreateDraft(memberCtx, member, expense.DraftInput{
		Category: "travel", Currency: "USD", AmountMinor: 12345,
		Merchant: "Acme Air", Description: "机票", OccurredAt: occurredAt,
		BudgetID: &travelBudget.ID,
	})
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, member, travel.ID, expense.ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.ApplyAction(adminCtx, admin, travel.ID, expense.ActionApprove, "同意")
	require.NoError(t, err)
	finalTravel, err := svc.ApplyAction(adminCtx, admin, travel.ID, expense.ActionReimburse, "已打款")
	require.NoError(t, err)
	require.Equal(t, expense.StatusReimbursed, finalTravel.Status)

	meals, err := svc.CreateDraft(memberCtx, member, expense.DraftInput{
		Category: "meals", Currency: "EUR", AmountMinor: 6000,
		Merchant: "Bistro", Description: "客户晚餐", OccurredAt: occurredAt,
		BudgetID: &mealsBudget.ID,
	})
	require.NoError(t, err)
	_, err = svc.ApplyAction(memberCtx, member, meals.ID, expense.ActionSubmit, "")
	require.NoError(t, err)
	finalMeals, err := svc.ApplyAction(adminCtx, admin, meals.ID, expense.ActionApprove, "同意")
	require.NoError(t, err)
	require.Equal(t, expense.StatusApproved, finalMeals.Status)

	dispatcher, err := outbox.NewDispatcher(outbox.DispatcherOptions{
		Database:       expenseDB,
		Publisher:      producer,
		Logger:         zap.NewNop(),
		BatchSize:      50,
		PollInterval:   10 * time.Millisecond,
		PublishTimeout: 10 * time.Second,
	})
	require.NoError(t, err)

	claimed := 0
	for i := 0; i < 10; i++ {
		n, err := dispatcher.DispatchOnce(context.Background())
		require.NoError(t, err)
		claimed += n
		if n == 0 {
			break
		}
	}
	assert.Equal(t, 5, claimed, "三次提交/审批/报销 + 一次提交/审批，共 5 个事件")
	assert.Zero(t, unpublishedOutbox(t, expenseDB, tenantID), "投递器返回后所有事件都必须已发布")

	eventually(t, 30*time.Second, func() bool {
		travelStatus, travelVersion, travelFound := factState(t, reportDB, tenantID, travel.ID)
		mealsStatus, mealsVersion, mealsFound := factState(t, reportDB, tenantID, meals.ID)
		return travelFound && mealsFound &&
			travelStatus == "reimbursed" && travelVersion == 4 &&
			mealsStatus == "approved" && mealsVersion == 3
	}, "报表读模型没有在超时内收敛到最终状态")

	assert.Equal(t, 5, processedCount(t, reportDB, tenantID), "每个事件只应被消费一次")

	issuer, err := auth.NewTokenIssuer(auth.IssuerConfig{
		Issuer: "expensio-e2e", HS256Secret: e2eJWTSecret,
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)
	token, _, err := issuer.IssueAccess(adminID, tenantID, tenantctx.RoleAdmin, uuid.New())
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	report.RegisterRoutes(engine, report.NewHandler(report.NewService(reportDB)), issuer)

	summary := fetchSummary(t, engine,
		token, "/api/v1/reports/expenses/summary?from="+day+"&to="+day)

	assert.Equal(t, report.ScopeAll, summary.Scope)
	assert.Equal(t, report.GroupByCategory, summary.GroupBy)

	require.Len(t, summary.CurrencyTotals, 2)
	totals := map[string]report.CurrencyTotal{}
	for _, ct := range summary.CurrencyTotals {
		totals[ct.Currency] = ct
	}
	assert.Equal(t, int64(12345), totals["USD"].AmountMinor)
	assert.Equal(t, int64(1), totals["USD"].ExpenseCount)
	assert.Equal(t, int64(6000), totals["EUR"].AmountMinor)
	assert.Equal(t, int64(1), totals["EUR"].ExpenseCount)

	require.Len(t, summary.Groups, 2)
	groups := map[string]report.GroupTotal{}
	for _, g := range summary.Groups {
		groups[g.Group+"|"+g.Currency] = g
	}
	assert.Equal(t, int64(12345), groups["travel|USD"].AmountMinor)
	assert.Equal(t, int64(6000), groups["meals|EUR"].AmountMinor)
}
