// gateway 是唯一对外入口：校验 JWT、限流、按前缀转发到五个业务服务。
// 监听 :8080。不连数据库——网关一旦能读写业务库，"服务边界"就名存实亡了。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/edgerunner-fris/expensio/internal/gateway"
	"github.com/edgerunner-fris/expensio/internal/platform/bootstrap"
	"github.com/edgerunner-fris/expensio/internal/platform/config"
)

func main() {
	os.Exit(bootstrap.Main(config.Options{
		Service:        "gateway",
		DefaultAddr:    ":8080",
		NeedsJWT:       true,
		NeedsRedis:     true,
		NeedsUpstreams: true,
	}, setup))
}

func setup(app *bootstrap.App) error {
	cfg := app.Config

	issuer, err := cfg.JWT.AuthIssuer()
	if err != nil {
		return fmt.Errorf("gateway: 初始化令牌校验器失败: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	app.OnStop(func() { _ = rdb.Close() })

	// Redis 是限流状态所在：启动时连不上就启动失败，避免以"无限流"状态对外服务。
	// 运行期故障则降级放行——见 gateway.allow 的说明。
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("gateway: 连接 Redis 失败: %w", err)
	}

	handler, err := gateway.New(gateway.Options{
		Parser: issuer,
		Upstreams: map[string]string{
			"identity":     cfg.Upstreams.Identity,
			"expense":      cfg.Upstreams.Expense,
			"billing":      cfg.Upstreams.Billing,
			"report":       cfg.Upstreams.Report,
			"notification": cfg.Upstreams.Notification,
		},
		Rules:   gateway.DefaultRules(),
		Limiter: gateway.NewRedisLimiter(rdb),
		Limits: gateway.Limits{
			AuthPerSecond: float64(cfg.RateLimit.AuthPerMinute) / 60,
			AuthBurst:     cfg.RateLimit.AuthBurst,
			APIPerSecond:  float64(cfg.RateLimit.APIPerMinute) / 60,
			APIBurst:      cfg.RateLimit.APIBurst,
		},
		Circuit: gateway.CircuitOptions{
			Failures:    cfg.CircuitBreaker.Failures,
			Interval:    cfg.CircuitBreaker.Interval,
			Timeout:     cfg.CircuitBreaker.Timeout,
			MaxRequests: cfg.CircuitBreaker.MaxRequests,
		},
		Logger: app.Logger,
	})
	if err != nil {
		return err
	}

	// 只注册这一条：所有 /api/v1 的请求由路由表决定去向，
	// 不在这里逐条登记——每加一个业务端点都要改网关，是最容易漏的一类改动。
	app.Server.Engine().Any("/api/v1/*path", handler.Handle)
	return nil
}
