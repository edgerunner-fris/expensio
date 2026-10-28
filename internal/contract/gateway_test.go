package contract

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/edgerunner-fris/expensio/internal/gateway"
)

// TestOpenAPIPathsRoutedByGateway 补上另一条漂移路径：服务注册了路由、OpenAPI
// 也写了，但网关没有对应的前缀规则。
//
// 这种漏配比前两种更隐蔽：服务健康检查正常，直连服务也正常，契约测试也过，
// 只有走网关的调用方拿到 404。approval-policies 就是这样被漏掉的。
func TestOpenAPIPathsRoutedByGateway(t *testing.T) {
	documented := specRoutes(t)
	rules := gateway.DefaultRules()

	routed := func(path string) bool {
		for _, r := range rules {
			if path == r.Prefix || strings.HasPrefix(path, r.Prefix+"/") {
				return true
			}
		}
		return false
	}

	var unrouted []string
	for r := range documented {
		// JWKS 由 identity 直接对外提供（文档里用单独的 server 描述），
		// 不经过网关。
		if r.Path == "/.well-known/jwks.json" {
			continue
		}
		if !routed(r.Path) {
			unrouted = append(unrouted, r.Method+" "+r.Path)
		}
	}
	sort.Strings(unrouted)
	assert.Empty(t, unrouted, "以下端点已写入 api/openapi.yaml，但网关没有转发规则")

	// 反向检查：规则前缀必须对应至少一个文档化端点，否则就是没人用的死规则，
	// 它只会把请求转发给下游的 404。
	var orphan []string
	for _, r := range rules {
		used := false
		for d := range documented {
			if d.Path == r.Prefix || strings.HasPrefix(d.Path, r.Prefix+"/") {
				used = true
				break
			}
		}
		if !used {
			orphan = append(orphan, r.Prefix)
		}
	}
	sort.Strings(orphan)
	assert.Empty(t, orphan, "以下网关规则没有对应的文档化端点")
}
