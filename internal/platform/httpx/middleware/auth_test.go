package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

type fakeParser struct {
	claims *auth.Claims
	err    error
}

func (p fakeParser) ParseAccess(string) (*auth.Claims, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.claims, nil
}

func tenantClaims(role tenantctx.Role) *auth.Claims {
	return &auth.Claims{
		TenantID: uuid.NewString(),
		Role:     string(role),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.NewString(),
		},
	}
}

func newEngine(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(mw)
	engine.GET("/probe", func(c *gin.Context) {
		t, ok := tenantctx.From(c.Request.Context())
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "缺少租户上下文"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tenant_id": t.ID.String(), "role": string(t.Role)})
	})
	return engine
}

func serve(engine *gin.Engine, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestAuthPopulatesTenantContext(t *testing.T) {
	claims := tenantClaims(tenantctx.RoleAdmin)
	engine := newEngine(Auth(fakeParser{claims: claims}))

	rec := serve(engine, "Bearer good-token")
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, claims.TenantID, body["tenant_id"])
	assert.Equal(t, "admin", body["role"])
}

func TestAuthRejectsBadCredentials(t *testing.T) {
	cases := map[string]struct {
		header string
		parser AccessParser
	}{
		"缺少令牌":    {"", fakeParser{claims: tenantClaims(tenantctx.RoleOwner)}},
		"非Bearer": {"Basic abc", fakeParser{claims: tenantClaims(tenantctx.RoleOwner)}},
		"空令牌":     {"Bearer ", fakeParser{claims: tenantClaims(tenantctx.RoleOwner)}},
		"解析失败":    {"Bearer bad", fakeParser{err: errors.New("签名不符")}},
		"缺租户声明":   {"Bearer bad", fakeParser{claims: &auth.Claims{Role: "owner", RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()}}}},
		"缺用户声明":   {"Bearer bad", fakeParser{claims: &auth.Claims{TenantID: uuid.NewString(), Role: "owner"}}},
		"非法角色":    {"Bearer bad", fakeParser{claims: &auth.Claims{TenantID: uuid.NewString(), Role: "superuser", RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()}}}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serve(newEngine(Auth(tc.parser)), tc.header)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Contains(t, rec.Body.String(), "auth.unauthorized")
		})
	}
}

func TestRequireRoleMatrix(t *testing.T) {
	roles := []tenantctx.Role{
		tenantctx.RoleOwner, tenantctx.RoleAdmin, tenantctx.RoleMember, tenantctx.RoleViewer,
	}

	for _, have := range roles {
		t.Run(string(have), func(t *testing.T) {
			engine := newEngine(Auth(fakeParser{claims: tenantClaims(have)}))
			// 在 Auth 之后追加角色网关：gin 的中间件顺序即执行顺序。
			engine.GET("/admin", RequireRole(tenantctx.RoleAdmin), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodGet, "/admin", nil)
			req.Header.Set("Authorization", "Bearer good")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)

			if have.AtLeast(tenantctx.RoleAdmin) {
				assert.Equal(t, http.StatusNoContent, rec.Code)
				return
			}
			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.Contains(t, rec.Body.String(), "auth.forbidden")
		})
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]struct {
		header string
		token  string
		ok     bool
	}{
		"标准":     {"Bearer abc.def.ghi", "abc.def.ghi", true},
		"小写前缀":   {"bearer abc", "abc", true},
		"前后空白":   {"Bearer   abc  ", "abc", true},
		"缺少前缀":   {"abc", "", false},
		"空值":     {"Bearer ", "", false},
		"令牌中有空格": {"Bearer a b", "", false},
		"其它方案":   {"Digest abc", "", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			token, ok := bearerToken(tc.header)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.token, token)
		})
	}
}
