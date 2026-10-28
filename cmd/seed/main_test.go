package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvitationToken(t *testing.T) {
	cases := map[string]struct {
		link  string
		token string
		ok    bool
	}{
		"前端邀请页":  {link: "https://expensio.example.com/invite/abc123", token: "abc123", ok: true},
		"接口路径":   {link: "http://localhost:5173/invitations/xyz-9", token: "xyz-9", ok: true},
		"非邀请路径":  {link: "https://expensio.example.com/settings/token", ok: false},
		"缺少令牌":   {link: "https://expensio.example.com/invite/", ok: false},
		"非法 URL": {link: "://bad", ok: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			token, err := invitationToken(tc.link)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.token, token)
		})
	}
}

func TestMonthRange(t *testing.T) {
	cases := map[string]struct {
		now  time.Time
		from string
		to   string
	}{
		"一月":    {now: time.Date(2026, 1, 15, 8, 0, 0, 0, time.UTC), from: "2026-01-01", to: "2026-01-31"},
		"二月平年":  {now: time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC), from: "2026-02-01", to: "2026-02-28"},
		"二月闰年":  {now: time.Date(2028, 2, 29, 23, 59, 0, 0, time.UTC), from: "2028-02-01", to: "2028-02-29"},
		"跨年十二月": {now: time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), from: "2026-12-01", to: "2026-12-31"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			from, to := monthRange(tc.now)
			assert.Equal(t, tc.from, from)
			assert.Equal(t, tc.to, to)
		})
	}
}

func TestParseBaseURL(t *testing.T) {
	got, err := parseBaseURL("http://localhost:8080/")
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:8080", got)

	for _, raw := range []string{
		"ftp://example.com",
		"example.com",
		"http://user:pass@example.com",
		"",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseBaseURL(raw)
			assert.Error(t, err)
		})
	}
}

func TestClientDoParsesErrorEnvelope(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"expense.state_conflict","message":"状态不允许","request_id":"req-1","details":{"status":"approved"}}`))
	}))
	defer srv.Close()

	c := &client{base: srv.URL, http: srv.Client()}
	err := c.do(context.Background(), http.MethodPost, "/expenses/1/submit", "tok", nil, nil)
	require.Error(t, err)
	assert.True(t, isStatus(err, http.StatusConflict))

	var ae *apiError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, "expense.state_conflict", ae.code)
	assert.Equal(t, "req-1", ae.requestID)
	assert.NotContains(t, err.Error(), "tok", "错误信息不能包含令牌")
	assert.Equal(t, "/api/v1/expenses/1/submit", gotPath)
	assert.Equal(t, "Bearer tok", gotAuth)
}

func TestClientDoDecodesData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "acme", in["tenant_slug"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"code":"ok","data":{"access_token":"a","profile":{"email":"e"}}}`))
	}))
	defer srv.Close()

	c := &client{base: srv.URL, http: srv.Client()}
	var sess session
	err := c.do(context.Background(), http.MethodPost, "/auth/login", "",
		map[string]any{"tenant_slug": "acme"}, &sess)
	require.NoError(t, err)
	assert.Equal(t, "a", sess.AccessToken)
	assert.Equal(t, "e", sess.Profile.Email)
}

func TestClientDoNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &client{base: srv.URL, http: srv.Client()}
	require.NoError(t, c.do(context.Background(), http.MethodDelete, "/x", "t", nil, nil))
}

func TestAPIErrorOmitsEmptyParts(t *testing.T) {
	err := &apiError{status: 401, code: "auth.invalid_credentials"}
	assert.Equal(t, "HTTP 401 auth.invalid_credentials", err.Error())
	assert.NotContains(t, strings.ToLower(err.Error()), "password")
}
