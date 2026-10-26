package httpx_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

func TestPageQueryNormalize(t *testing.T) {
	cases := []struct {
		name     string
		in       httpx.PageQuery
		wantPage int
		wantSize int
	}{
		{"默认值", httpx.PageQuery{}, 1, httpx.DefaultPageSize},
		{"负数收敛", httpx.PageQuery{Page: -3, PageSize: -1}, 1, httpx.DefaultPageSize},
		{"超上限截断", httpx.PageQuery{Page: 2, PageSize: 5000}, 2, httpx.MaxPageSize},
		{"合法值原样", httpx.PageQuery{Page: 3, PageSize: 50}, 3, 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.in
			q.Normalize()
			assert.Equal(t, tc.wantPage, q.Page)
			assert.Equal(t, tc.wantSize, q.PageSize)
		})
	}
}

func TestOffset(t *testing.T) {
	assert.Equal(t, 0, httpx.PageQuery{Page: 1, PageSize: 20}.Offset())
	assert.Equal(t, 40, httpx.PageQuery{Page: 3, PageSize: 20}.Offset())
}

func TestNewPageSerializesEmptyItemsAsArray(t *testing.T) {
	page := httpx.NewPage[string](nil, 0, httpx.PageQuery{Page: 1, PageSize: 10})

	body, err := json.Marshal(page)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"items":[]`)
	assert.NotContains(t, string(body), `"items":null`)
}
