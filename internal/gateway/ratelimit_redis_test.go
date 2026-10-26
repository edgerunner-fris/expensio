package gateway

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

// TestRedisLimiterAgainstRealRedis 验证 Lua 脚本本身：
// 单元测试里的假限流器只能证明"网关调用了限流器"，证明不了桶算得对。
func TestRedisLimiterAgainstRealRedis(t *testing.T) {
	addr := testenv.StartRedis(t)
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })

	limiter := NewRedisLimiter(client)
	ctx := context.Background()

	newKey := func() string { return "expensio:rl:test:" + uuid.NewString() }

	t.Run("令牌耗尽后拒绝并给出等待时间", func(t *testing.T) {
		key := newKey()

		for i := 0; i < 3; i++ {
			d, err := limiter.Allow(ctx, key, 0.001, 3)
			require.NoError(t, err)
			assert.True(t, d.Allowed, "第 %d 次应放行", i+1)
		}

		d, err := limiter.Allow(ctx, key, 0.001, 3)
		require.NoError(t, err)
		assert.False(t, d.Allowed)
		assert.Greater(t, d.RetryAfter, time.Duration(0))
	})

	t.Run("按速率补充令牌", func(t *testing.T) {
		key := newKey()

		for i := 0; i < 2; i++ {
			d, err := limiter.Allow(ctx, key, 20, 2)
			require.NoError(t, err)
			require.True(t, d.Allowed)
		}

		// 20 个/秒意味着 150ms 足以补满 2 个令牌，留出足够的调度余量。
		time.Sleep(150 * time.Millisecond)

		d, err := limiter.Allow(ctx, key, 20, 2)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "经过补充窗口后应重新放行")
	})

	t.Run("不同键互不影响", func(t *testing.T) {
		keyA, keyB := newKey(), newKey()

		for i := 0; i < 2; i++ {
			_, err := limiter.Allow(ctx, keyA, 0.001, 2)
			require.NoError(t, err)
		}
		denied, err := limiter.Allow(ctx, keyA, 0.001, 2)
		require.NoError(t, err)
		require.False(t, denied.Allowed)

		allowed, err := limiter.Allow(ctx, keyB, 0.001, 2)
		require.NoError(t, err)
		assert.True(t, allowed.Allowed, "另一个租户/来源不应被牵连")
	})

	t.Run("并发请求不会超发", func(t *testing.T) {
		key := newKey()
		const (
			burst    = 5
			callers  = 20
			nearZero = 0.001
		)

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			allowed int
		)
		start := make(chan struct{})
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				d, err := limiter.Allow(ctx, key, nearZero, burst)
				if err != nil {
					return
				}
				if d.Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()

		// 脚本在 Redis 内原子执行；补令牌速率极低，可以近似认为窗口内只有 burst 个。
		assert.Equal(t, burst, allowed, "并发下放行数量必须等于桶容量")
	})

	t.Run("键会过期，不会永久占内存", func(t *testing.T) {
		key := newKey()
		_, err := limiter.Allow(ctx, key, 1, 1)
		require.NoError(t, err)

		ttl, err := client.PTTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, time.Duration(0), "桶状态必须带 TTL")
	})

	t.Run("非法参数不制造故障", func(t *testing.T) {
		for _, tc := range []struct {
			rate  float64
			burst int
		}{
			{rate: 0, burst: 1},
			{rate: 1, burst: 0},
			{rate: -1, burst: -1},
		} {
			d, err := limiter.Allow(ctx, newKey(), tc.rate, tc.burst)
			require.NoError(t, err)
			assert.True(t, d.Allowed, "配置缺失时应放行而不是全拒")
		}
	})
}
