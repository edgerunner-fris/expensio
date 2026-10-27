package expense

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestCommentsLifecycle(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)

	tenant, submitter, reviewer := uuid.New(), uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, submitter, tenantctx.RoleMember)
	submitterActor := Actor{UserID: submitter, Role: tenantctx.RoleMember}
	reviewerActor := Actor{UserID: reviewer, Role: tenantctx.RoleAdmin}

	e, err := svc.CreateDraft(ctx, submitterActor, validInput())
	require.NoError(t, err)

	// 空白评论必须拒绝，不能存一条只有空格、前端显示为空的记录。
	_, err = svc.AddComment(ctx, submitterActor, e.ID, CommentInput{Body: "   "})
	require.Error(t, err)

	first, err := svc.AddComment(ctx, submitterActor, e.ID, CommentInput{Body: "  发票在附件里  "})
	require.NoError(t, err)
	assert.Equal(t, "发票在附件里", first.Body, "首尾空白应被裁剪")
	assert.Equal(t, submitter, first.AuthorID)

	_, err = svc.AddComment(ctx, reviewerActor, e.ID, CommentInput{Body: "已看到，稍后审批"})
	require.NoError(t, err)

	// 提交后仍可评论：沟通不应被状态机挡住。
	_, err = svc.ApplyAction(ctx, submitterActor, e.ID, ActionSubmit, "")
	require.NoError(t, err)
	_, err = svc.AddComment(ctx, reviewerActor, e.ID, CommentInput{Body: "金额核对中"})
	require.NoError(t, err)

	list, err := svc.ListComments(ctx, reviewerActor, e.ID)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, "发票在附件里", list[0].Body)

	// 别人删不掉；作者本人可以；管理岗可以删除任意一条。
	err = svc.DeleteComment(ctx, reviewerActor, e.ID, first.ID)
	assert.NoError(t, err, "管理岗可以删除治理性内容")

	second := list[1]
	other := Actor{UserID: uuid.New(), Role: tenantctx.RoleMember}
	err = svc.DeleteComment(ctx, other, e.ID, second.ID)
	assert.Equal(t, 403, httpStatus(err))

	err = svc.DeleteComment(ctx, reviewerActor, e.ID, second.ID)
	require.NoError(t, err, "作者本人可以删除自己的评论")

	list, err = svc.ListComments(ctx, submitterActor, e.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestCommentLengthAndVisibility(t *testing.T) {
	database := newDB(t)
	svc := newService(t, database)

	tenant, owner := uuid.New(), uuid.New()
	ctx := tenantCtx(t, tenant, owner, tenantctx.RoleMember)
	ownerActor := Actor{UserID: owner, Role: tenantctx.RoleMember}

	e, err := svc.CreateDraft(ctx, ownerActor, validInput())
	require.NoError(t, err)

	_, err = svc.AddComment(ctx, ownerActor, e.ID, CommentInput{Body: strings.Repeat("字", maxCommentBodyRunes+1)})
	assert.Equal(t, 400, httpStatus(err))

	// 其他成员的普通单据不可见（member 只能看自己的），评论也不可见。
	otherMember := Actor{UserID: uuid.New(), Role: tenantctx.RoleMember}
	_, err = svc.ListComments(ctx, otherMember, e.ID)
	assert.Equal(t, 404, httpStatus(err), "无权查看的单据按不存在处理")
	_, err = svc.AddComment(ctx, otherMember, e.ID, CommentInput{Body: "hi"})
	assert.Equal(t, 404, httpStatus(err))
}
