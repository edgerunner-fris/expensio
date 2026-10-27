package expense

import (
	"errors"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

// actionRequest 是所有审批动作共享的请求体：只有可选意见。
// 目标状态由 URL 里的动作决定，客户端不能直接写 status 字段。
type actionRequest struct {
	Comment string `json:"comment"`
}

// approvalView 是一级批准的对外形态。
type approvalView struct {
	Level     int16     `json:"level"`
	ActorID   uuid.UUID `json:"actor_id"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListApprovals 返回各级批准与当前进度。
// required/approved 级数来自费用单快照，前端据此显示"1/2 已批准"。
func (h *Handler) ListApprovals(c *gin.Context) {
	actor, err := actorFrom(c.Request.Context())
	if err != nil {
		httpx.Error(c, err)
		return
	}
	id, err := expenseID(c)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	records, e, err := h.svc.ListApprovals(c.Request.Context(), actor, id)
	if err != nil {
		httpx.Error(c, err)
		return
	}
	items := make([]approvalView, 0, len(records))
	for _, r := range records {
		items = append(items, approvalView{
			Level: r.Level, ActorID: r.ActorID, Comment: r.Comment, CreatedAt: r.CreatedAt,
		})
	}
	httpx.OK(c, gin.H{
		"required_levels": e.RequiredApprovalLevels,
		"approved_levels": e.ApprovalLevel,
		"items":           items,
	})
}

// actionHandler 为每个动作生成一个端点。
// 五个端点共用同一段解析与错误映射，避免出现"submit 校验了、cancel 忘了"这类漂移。
func (h *Handler) actionHandler(action Action) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := actorFrom(c.Request.Context())
		if err != nil {
			httpx.Error(c, err)
			return
		}
		id, err := expenseID(c)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		var in actionRequest
		// 审批意见是可选的，允许完全空体；其余解析错误仍然拒绝。
		if bindErr := c.ShouldBindJSON(&in); bindErr != nil && !errors.Is(bindErr, io.EOF) {
			httpx.Error(c, apperr.InvalidArgument("请求体格式非法"))
			return
		}
		e, err := h.svc.ApplyAction(c.Request.Context(), actor, id, action, in.Comment)
		if err != nil {
			httpx.Error(c, err)
			return
		}
		httpx.OK(c, newExpenseView(e))
	}
}
