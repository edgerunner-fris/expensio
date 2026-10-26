package expense

import (
	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx/middleware"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// RegisterRoutes 注册 expense 的对外端点。
//
// 所有端点都先过 Auth：expense 不假设网关已经校验过令牌。
// 角色与所有权规则在领域层（AuthorizeDraftWrite / CanView），
// 这里只用中间件表达"必须是已认证用户"，不重复角色判断。
func RegisterRoutes(r gin.IRouter, h *Handler, parser middleware.AccessParser) {
	v1 := r.Group("/api/v1")

	authed := v1.Group("")
	authed.Use(middleware.Auth(parser))

	authed.POST("/expenses", h.CreateExpense)
	authed.GET("/expenses", h.ListExpenses)
	authed.GET("/expenses/:expenseID", h.GetExpense)
	authed.PUT("/expenses/:expenseID", h.UpdateExpense)
	authed.DELETE("/expenses/:expenseID", h.DeleteExpense)

	// 审批动作各自一个端点。谁能执行哪个动作由领域层 AuthorizeAction 决定：
	// 提交限本人，审批/驳回/报销限 admin 及以上且禁止自审，撤销限本人或管理岗。
	authed.POST("/expenses/:expenseID/submit", h.actionHandler(ActionSubmit))
	authed.POST("/expenses/:expenseID/approve", h.actionHandler(ActionApprove))
	authed.POST("/expenses/:expenseID/reject", h.actionHandler(ActionReject))
	authed.POST("/expenses/:expenseID/cancel", h.actionHandler(ActionCancel))
	authed.POST("/expenses/:expenseID/reimburse", h.actionHandler(ActionReimburse))

	// 预算对全租户可读（提交费用时要知道有哪些预算），
	// 写操作只允许 owner/admin：在网关与中间件各挡一次，service 里还有第三次判断。
	authed.GET("/budgets", h.ListBudgets)
	authed.GET("/budgets/:budgetID", h.GetBudget)

	admin := authed.Group("")
	admin.Use(middleware.RequireRole(tenantctx.RoleAdmin))
	admin.POST("/budgets", h.CreateBudget)
	admin.PUT("/budgets/:budgetID", h.UpdateBudget)
	admin.DELETE("/budgets/:budgetID", h.DeleteBudget)
}
