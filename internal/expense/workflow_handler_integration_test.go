package expense

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestHTTPWorkflow(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant := uuid.New()
	admin, member, owner, viewer := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	adminToken := tokenFor(t, issuer, tenant, admin, tenantctx.RoleAdmin)
	memberToken := tokenFor(t, issuer, tenant, member, tenantctx.RoleMember)
	ownerToken := tokenFor(t, issuer, tenant, owner, tenantctx.RoleOwner)
	viewerToken := tokenFor(t, issuer, tenant, viewer, tenantctx.RoleViewer)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/budgets", adminToken, newBudgetInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	budget := decode[budgetView](t, rec)

	rec = doRequest(t, engine, http.MethodPost, "/api/v1/expenses", memberToken, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	expense := decode[expenseView](t, rec)
	path := "/api/v1/expenses/" + expense.ID.String()

	// 未认证请求进不了审批端点。
	rec = doRequest(t, engine, http.MethodPost, path+"/submit", "", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// viewer 是只读角色。
	rec = doRequest(t, engine, http.MethodPost, path+"/submit", viewerToken, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 空请求体允许（意见可选）。
	rec = doRequest(t, engine, http.MethodPost, path+"/submit", memberToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	submitted := decode[expenseView](t, rec)
	assert.Equal(t, string(StatusSubmitted), submitted.Status)
	require.NotNil(t, submitted.BudgetID)
	assert.Equal(t, budget.ID, *submitted.BudgetID)

	// 重复提交必须 409，而不是幂等成功。
	rec = doRequest(t, engine, http.MethodPost, path+"/submit", memberToken, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, CodeStateConflict, decodeError(t, rec).Code)

	// 驳回不带理由 → 400；审批意见是驳回的必填项。
	rec = doRequest(t, engine, http.MethodPost, path+"/reject", adminToken, map[string]any{})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = doRequest(t, engine, http.MethodPost, path+"/approve", adminToken, map[string]any{"comment": "同意"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	approved := decode[expenseView](t, rec)
	assert.Equal(t, string(StatusApproved), approved.Status)
	assert.Equal(t, admin, *approved.DecidedBy)

	rec = doRequest(t, engine, http.MethodPost, path+"/reimburse", adminToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(StatusReimbursed), decode[expenseView](t, rec).Status)

	// 通过 HTTP 也无法自审。
	rec = doRequest(t, engine, http.MethodPost, "/api/v1/expenses", ownerToken, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	ownerExpense := decode[expenseView](t, rec)
	ownerPath := "/api/v1/expenses/" + ownerExpense.ID.String()
	rec = doRequest(t, engine, http.MethodPost, ownerPath+"/submit", ownerToken, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, engine, http.MethodPost, ownerPath+"/approve", ownerToken, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, CodeSelfApproval, decodeError(t, rec).Code)
}

func TestHTTPWorkflowBadRequests(t *testing.T) {
	database := newDB(t)
	engine, issuer := newHTTPTestServer(t, database)
	tenant, user := uuid.New(), uuid.New()
	token := tokenFor(t, issuer, tenant, user, tenantctx.RoleMember)

	rec := doRequest(t, engine, http.MethodPost, "/api/v1/expenses", token, validInput())
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	path := "/api/v1/expenses/" + decode[expenseView](t, rec).ID.String()

	cases := []struct {
		name string
		path string
		body any
	}{
		{"非法 ID", "/api/v1/expenses/not-a-uuid/submit", nil},
		{"请求体不是对象", path + "/submit", "oops"},
		{"意见类型错误", path + "/cancel", map[string]any{"comment": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, engine, http.MethodPost, tc.path, token, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.NotEmpty(t, decodeError(t, rec).Code)
		})
	}
}
