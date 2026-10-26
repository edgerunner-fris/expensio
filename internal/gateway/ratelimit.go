package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Decision 是一次限流判定的结果。
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter 抽象令牌桶，便于测试与故障降级。
type Limiter interface {
	Allow(ctx context.Context, key string, perSecond float64, burst int) (Decision, error)
}

// tokenBucketScript 是标准的令牌桶，整个过程在 Redis 内一次原子完成。
//
// 用 Redis 的 TIME 而不是客户端时间：多个网关实例的时钟不可能完全一致，
// 混用会让同一个桶的补充速率忽快忽慢。
// 返回 {是否放行, 剩余令牌, 需要等待的毫秒数}。
var tokenBucketScript = redis.NewScript(`
local key    = KEYS[1]
local rate   = tonumber(ARGV[1])   -- 每毫秒补充的令牌数
local burst  = tonumber(ARGV[2])   -- 桶容量
local cost   = tonumber(ARGV[3])   -- 本次消耗

local t   = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local data   = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts     = tonumber(data[2])
if tokens == nil or ts == nil then
  tokens = burst
  ts     = now
end

local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + elapsed * rate)

local allowed = 0
local wait_ms = 0
if tokens >= cost then
  tokens  = tokens - cost
  allowed = 1
else
  wait_ms = math.ceil((cost - tokens) / rate)
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
-- 桶装满后不再需要状态：TTL 至少 1 秒，防止极短的窗口把 key 反复创建与销毁。
local ttl = math.ceil(burst / rate * 2)
if ttl < 1000 then ttl = 1000 end
redis.call('PEXPIRE', key, ttl)

return {allowed, math.floor(tokens), wait_ms}
`)

// RedisLimiter 是 Limiter 的分布式实现：限流状态在 Redis 里，
// 所以网关扩容到 N 个实例时，总量仍然是 N 个实例共享的一份。
type RedisLimiter struct {
	client redis.UniversalClient
}

func NewRedisLimiter(client redis.UniversalClient) *RedisLimiter {
	return &RedisLimiter{client: client}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string, perSecond float64, burst int) (Decision, error) {
	if perSecond <= 0 || burst <= 0 {
		// 配置缺失时放行而不是全拒：限流的目的是保护系统，不是制造故障。
		return Decision{Allowed: true}, nil
	}

	raw, err := tokenBucketScript.Run(ctx, l.client, []string{key},
		perSecond/1000, burst, 1,
	).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("gateway: 执行限流脚本失败: %w", err)
	}
	if len(raw) != 3 {
		return Decision{}, fmt.Errorf("gateway: 限流脚本返回了 %d 个值，期望 3 个", len(raw))
	}

	return Decision{
		Allowed:    raw[0] == 1,
		Remaining:  int(raw[1]),
		RetryAfter: time.Duration(raw[2]) * time.Millisecond,
	}, nil
}
