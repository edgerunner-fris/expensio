package apperr_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

func TestNotFoundMatchesByCode(t *testing.T) {
	err := apperr.NotFound("费用")

	assert.True(t, errors.Is(err, apperr.NotFound("预算")))
	assert.Equal(t, http.StatusNotFound, apperr.Status(err))
	assert.Equal(t, apperr.CodeNotFound, apperr.Code(err))
}

func TestWithCauseKeepsPublicMessageAndUnwraps(t *testing.T) {
	err := apperr.Internal(io.EOF)

	assert.True(t, errors.Is(err, io.EOF))
	assert.Equal(t, http.StatusInternalServerError, apperr.Status(err))
	assert.Equal(t, "服务内部错误，请稍后重试", apperr.Message(err))
	assert.Contains(t, err.Error(), "EOF")
}

func TestWithDetailDoesNotMutateOriginal(t *testing.T) {
	base := apperr.InvalidArgument("金额不合法")
	withDetail := base.WithDetail("field", "amount")

	assert.Nil(t, base.Details())
	require.NotNil(t, withDetail.Details())
	assert.Equal(t, "amount", withDetail.Details()["field"])
	assert.Equal(t, base.Message(), withDetail.Message())
}

func TestUnknownErrorIsInternal(t *testing.T) {
	err := errors.New("connection reset")

	assert.Equal(t, http.StatusInternalServerError, apperr.Status(err))
	assert.Equal(t, apperr.CodeInternal, apperr.Code(err))
	assert.Nil(t, apperr.Details(err))
	assert.False(t, apperr.IsCode(err, apperr.CodeConflict))
}

func TestWrappedErrorStillResolves(t *testing.T) {
	err := fmt.Errorf("加载费用: %w", apperr.Forbidden("无权访问该费用"))

	assert.Equal(t, http.StatusForbidden, apperr.Status(err))
	assert.Equal(t, apperr.CodeForbidden, apperr.Code(err))
	assert.True(t, apperr.IsCode(err, apperr.CodeForbidden))
}
