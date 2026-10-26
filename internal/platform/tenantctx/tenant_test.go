package tenantctx_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func TestWithAndFromRoundTrip(t *testing.T) {
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	userID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	ctx, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID: tenantID, Slug: "acme", UserID: userID, Role: tenantctx.RoleAdmin,
	})
	require.NoError(t, err)

	got, ok := tenantctx.From(ctx)
	require.True(t, ok)
	assert.Equal(t, tenantID, got.ID)
	assert.Equal(t, "acme", got.Slug)
	assert.Equal(t, userID, got.UserID)
	assert.Equal(t, tenantctx.RoleAdmin, got.Role)
	assert.Equal(t, tenantID, tenantctx.ID(ctx))
}

func TestMissingContextReturnsZeroValues(t *testing.T) {
	_, ok := tenantctx.From(context.Background())
	assert.False(t, ok)
	assert.Equal(t, uuid.Nil, tenantctx.ID(context.Background()))
}

func TestRejectsEmptyTenantID(t *testing.T) {
	_, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		UserID: uuid.New(), Role: tenantctx.RoleMember,
	})
	require.Error(t, err)
}

func TestRejectsInvalidRole(t *testing.T) {
	_, err := tenantctx.With(context.Background(), tenantctx.Tenant{
		ID: uuid.New(), UserID: uuid.New(), Role: "SUPERUSER",
	})
	require.Error(t, err)
}
