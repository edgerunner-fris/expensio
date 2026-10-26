// Package health 提供存活与就绪两类探针。
//
// 两者必须分离：
//   - /healthz 只回答"进程还在吗"，不碰任何下游。liveness 探针查数据库会把
//     一次 DB 抖动放大成整个 Deployment 的连环重启。
//   - /readyz 回答"现在能正确干活吗"，检查本实例的关键依赖，失败只影响流量摘除。
package health

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/version"
)

const (
	// checkTimeout 是整批依赖检查的总预算，不是每个检查各 2s。
	checkTimeout = 2 * time.Second
	maxErrLen    = 160
)

// Checker 是一个可命名的依赖探针。
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

type funcChecker struct {
	name string
	fn   func(ctx context.Context) error
}

func (f funcChecker) Name() string                    { return f.name }
func (f funcChecker) Check(ctx context.Context) error { return f.fn(ctx) }

// Check 用函数构造探针，避免为每个依赖定义一个类型。
func Check(name string, fn func(ctx context.Context) error) Checker {
	return funcChecker{name: name, fn: fn}
}

// Registry 持有某个服务的全部依赖探针。
type Registry struct {
	service string

	mu       sync.RWMutex
	checkers []Checker
}

func New(service string) *Registry {
	return &Registry{service: service}
}

func (r *Registry) Add(c Checker) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers = append(r.checkers, c)
}

type probeResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks,omitempty"`
}

func (r *Registry) probe(status string, checks map[string]string) probeResponse {
	return probeResponse{
		Status:  status,
		Service: r.service,
		Version: version.Version,
		Checks:  checks,
	}
}

// Live 永远返回 200：能执行到这里说明进程、调度器和 HTTP 栈都正常。
func (r *Registry) Live(c *gin.Context) {
	c.JSON(http.StatusOK, r.probe("ok", nil))
}

// Ready 并行检查全部依赖，任一失败即 503。
// 返回的 checks 便于人肉定位是哪一个依赖挂了。
func (r *Registry) Ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), checkTimeout)
	defer cancel()

	r.mu.RLock()
	checkers := make([]Checker, len(r.checkers))
	copy(checkers, r.checkers)
	r.mu.RUnlock()

	results := make(map[string]string, len(checkers))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		bad int
	)

	for _, ck := range checkers {
		wg.Add(1)
		go func(ck Checker) {
			defer wg.Done()
			err := ck.Check(ctx)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				bad++
				results[ck.Name()] = truncate(err.Error())
				return
			}
			results[ck.Name()] = "ok"
		}(ck)
	}
	wg.Wait()

	if bad > 0 {
		c.JSON(http.StatusServiceUnavailable, r.probe("unavailable", results))
		return
	}

	c.JSON(http.StatusOK, r.probe("ok", results))
}

func truncate(s string) string {
	if len(s) <= maxErrLen {
		return s
	}
	return fmt.Sprintf("%s…", s[:maxErrLen])
}
