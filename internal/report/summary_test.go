package report

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func actor(role tenantctx.Role) tenantctx.Tenant {
	return tenantctx.Tenant{ID: uuid.New(), UserID: uuid.New(), Role: role}
}

func validQuery() SummaryQuery {
	return SummaryQuery{From: "2026-10-01", To: "2026-10-31"}
}

func TestResolveScopeDefaults(t *testing.T) {
	tests := []struct {
		role     tenantctx.Role
		request  string
		want     Scope
		wantCode string
	}{
		{tenantctx.RoleOwner, "", ScopeAll, ""},
		{tenantctx.RoleAdmin, "", ScopeAll, ""},
		{tenantctx.RoleMember, "", ScopeMine, ""},
		{tenantctx.RoleViewer, "", ScopeMine, ""},
		{tenantctx.RoleAdmin, "mine", ScopeMine, ""},
		{tenantctx.RoleMember, "mine", ScopeMine, ""},
		{tenantctx.RoleMember, "all", "", apperr.CodeForbidden},
		{tenantctx.RoleViewer, "all", "", apperr.CodeForbidden},
		{tenantctx.RoleAdmin, "everything", "", apperr.CodeInvalidArgument},
	}

	for _, tt := range tests {
		t.Run(string(tt.role)+"/"+tt.request, func(t *testing.T) {
			got, err := resolveScope(actor(tt.role), tt.request)
			if tt.wantCode != "" {
				require.Error(t, err)
				e, ok := apperr.As(err)
				require.True(t, ok)
				assert.Equal(t, tt.wantCode, e.Code())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewSummaryFilter(t *testing.T) {
	admin := actor(tenantctx.RoleAdmin)
	member := actor(tenantctx.RoleMember)

	t.Run("to 是闭区间", func(t *testing.T) {
		f, err := newSummaryFilter(admin, validQuery())
		require.NoError(t, err)
		assert.Equal(t, ScopeAll, f.Scope)
		assert.Equal(t, GroupByCategory, f.GroupBy)
		assert.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), f.From)
		assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), f.ToExclusive)
		assert.Zero(t, f.SubmitterID, "管理员默认看全租户")
	})

	t.Run("成员默认过滤到本人", func(t *testing.T) {
		f, err := newSummaryFilter(member, validQuery())
		require.NoError(t, err)
		assert.Equal(t, ScopeMine, f.Scope)
		assert.Equal(t, member.UserID, f.SubmitterID)
	})

	t.Run("分组维度", func(t *testing.T) {
		q := validQuery()
		q.GroupBy = "status"
		f, err := newSummaryFilter(admin, q)
		require.NoError(t, err)
		assert.Equal(t, GroupByStatus, f.GroupBy)
	})

	t.Run("非法参数", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*SummaryQuery)
		}{
			{"缺少 from", func(q *SummaryQuery) { q.From = "" }},
			{"缺少 to", func(q *SummaryQuery) { q.To = "" }},
			{"日期格式错误", func(q *SummaryQuery) { q.From = "2026/10/01" }},
			{"from 晚于 to", func(q *SummaryQuery) { q.From = "2026-11-01" }},
			{"范围超过一年", func(q *SummaryQuery) {
				q.From = "2025-01-01"
				q.To = "2026-01-02"
			}},
			{"未知分组", func(q *SummaryQuery) { q.GroupBy = "submitter" }},
			{"非法状态", func(q *SummaryQuery) { q.Status = "draft" }},
			{"非法类别", func(q *SummaryQuery) { q.Category = "gadgets" }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				q := validQuery()
				tt.mutate(&q)
				_, err := newSummaryFilter(admin, q)
				require.Error(t, err)
				e, ok := apperr.As(err)
				require.True(t, ok)
				assert.Equal(t, apperr.CodeInvalidArgument, e.Code())
			})
		}
	})

	t.Run("一年整可以查询", func(t *testing.T) {
		q := SummaryQuery{From: "2025-01-01", To: "2025-12-31"}
		_, err := newSummaryFilter(admin, q)
		require.NoError(t, err, "闰年以外的完整自然年必须允许")
	})
}
