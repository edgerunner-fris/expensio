// Package contract 把 HTTP 契约（api/openapi.yaml）与真实注册的路由锁在一起。
//
// 没有这层检查时，OpenAPI 会随着代码演进悄悄过期：新端点没写进文档，
// 或者删掉的端点还留在文档里。两种漂移都不会让任何测试失败，
// 却会让"照着文档对接"的调用方踩空。
package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/edgerunner-fris/expensio/internal/billing"
	"github.com/edgerunner-fris/expensio/internal/expense"
	"github.com/edgerunner-fris/expensio/internal/identity"
	"github.com/edgerunner-fris/expensio/internal/notification"
	"github.com/edgerunner-fris/expensio/internal/report"
)

// route 是规范化后的一个端点：方法 + 去掉参数名的路径。
type route struct {
	Method string
	Path   string
}

var (
	ginParam = regexp.MustCompile(`[:*][A-Za-z0-9_]+`)
	oasParam = regexp.MustCompile(`\{[^}]+\}`)
)

func normalize(method, path string) route {
	path = ginParam.ReplaceAllString(path, "{}")
	path = oasParam.ReplaceAllString(path, "{}")
	return route{Method: strings.ToUpper(method), Path: path}
}

// registeredRoutes 真实启动五个服务的路由注册函数，不构造任何依赖：
// RegisterRoutes 只在注册期使用 handler 指针，服务与仓储为 nil 是安全的。
// 处理器永远不会被调用——这里只关心路由表。
func registeredRoutes(t *testing.T) map[route]bool {
	t.Helper()
	gin.SetMode(gin.TestMode)

	engines := map[string]func(gin.IRouter){
		"identity": func(r gin.IRouter) { identity.RegisterRoutes(r, identity.NewHandler(nil), nil) },
		"expense":  func(r gin.IRouter) { expense.RegisterRoutes(r, expense.NewHandler(nil), nil) },
		"billing":  func(r gin.IRouter) { billing.RegisterRoutes(r, billing.NewHandler(nil), nil) },
		"report":   func(r gin.IRouter) { report.RegisterRoutes(r, report.NewHandler(nil), nil) },
		"notification": func(r gin.IRouter) {
			notification.RegisterRoutes(r, notification.NewHandler(nil), nil)
		},
	}

	out := make(map[route]bool)
	for name, register := range engines {
		engine := gin.New()
		register(engine)
		for _, info := range engine.Routes() {
			// /api/v1/internal/* 是服务间接口，不属于公开契约。
			if strings.Contains(info.Path, "/internal/") {
				continue
			}
			r := normalize(info.Method, info.Path)
			if out[r] {
				t.Errorf("%s: 路由重复注册 %s %s", name, r.Method, r.Path)
			}
			out[r] = true
		}
	}
	return out
}

type specDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `yaml:"operationId"`
	} `yaml:"paths"`
}

func specRoutes(t *testing.T) map[route]bool {
	t.Helper()

	path := filepath.Join("..", "..", "api", "openapi.yaml")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "读取 %s 失败", path)

	var doc specDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Paths)

	out := make(map[route]bool)
	for p, ops := range doc.Paths {
		for method, op := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			require.NotEmpty(t, op.OperationID, "%s %s 缺少 operationId", method, p)
			out[normalize(method, p)] = true
		}
	}
	return out
}

// TestOpenAPIMatchesRegisteredRoutes 要求契约与代码双向一致：
// 每个注册的公开路由都在文档里，文档里也没有不存在的路由。
func TestOpenAPIMatchesRegisteredRoutes(t *testing.T) {
	registered := registeredRoutes(t)
	documented := specRoutes(t)

	var missing, stale []string
	for r := range registered {
		if !documented[r] {
			missing = append(missing, r.Method+" "+r.Path)
		}
	}
	for r := range documented {
		if !registered[r] {
			stale = append(stale, r.Method+" "+r.Path)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	assert.Empty(t, missing, "以下路由已注册但未写入 api/openapi.yaml")
	assert.Empty(t, stale, "以下路由写在 api/openapi.yaml 但代码未注册")
}
