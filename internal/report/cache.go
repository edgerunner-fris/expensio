package report

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// dashboardTTL 是仪表盘缓存的存活时间。版本号负责正确性，TTL 只负责
// 清理不再被读取的旧键。
const dashboardTTL = 5 * time.Minute

// DashboardCache 用租户级版本号缓存仪表盘。
//
// 缓存不是正确性依赖：任何 Redis 错误都返回"未命中"，由调用方现算。
type DashboardCache struct {
	rdb *redis.Client
	log *zap.Logger
}

func NewDashboardCache(rdb *redis.Client, log *zap.Logger) *DashboardCache {
	if log == nil {
		log = zap.NewNop()
	}
	return &DashboardCache{rdb: rdb, log: log}
}

func versionKey(tenantID uuid.UUID) string {
	return "report:version:" + tenantID.String()
}

// dashboardKey 生成缓存键。
//
// scope=mine 的结果只属于提交人本人，键里必须带 submitterID：
// 同一租户的两个成员在同一天范围里查询，若共用键就会读到对方的单据。
// scope=all 的 submitterID 为零值，同一租户的管理岗共享结果。
func dashboardKey(version int64, tenantID, submitterID uuid.UUID, scope Scope, from, to string) string {
	view := string(scope)
	if submitterID != uuid.Nil {
		view += ":" + submitterID.String()
	}
	return fmt.Sprintf("report:dashboard:v%d:%s:%s:%s:%s",
		version, tenantID, view, from, to)
}

// Lookup 读取缓存。第二个返回值是键（即使未命中也可能返回，
// 供现算后 Store 使用）；第三个返回值表示是否命中。
func (c *DashboardCache) Lookup(ctx context.Context, tenantID, submitterID uuid.UUID,
	scope Scope, from, to string) (Dashboard, string, bool) {
	if c == nil || c.rdb == nil {
		return Dashboard{}, "", false
	}

	version, err := c.rdb.Get(ctx, versionKey(tenantID)).Int64()
	switch {
	case err == redis.Nil:
		version = 0
	case err != nil:
		c.log.Warn("report: 读取仪表盘缓存版本失败，回退现算",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return Dashboard{}, "", false
	}

	key := dashboardKey(version, tenantID, submitterID, scope, from, to)
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return Dashboard{}, key, false
	}
	if err != nil {
		c.log.Warn("report: 读取仪表盘缓存失败，回退现算",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return Dashboard{}, key, false
	}

	var out Dashboard
	if err := json.Unmarshal(raw, &out); err != nil {
		c.log.Warn("report: 仪表盘缓存内容损坏，忽略",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return Dashboard{}, key, false
	}
	return out, key, true
}

// Store 写入缓存。key 来自 Lookup，因此版本号在读写之间保持一致：
// 即使期间有新事件把版本号推进，这次写入也只会落到旧键上，不会被读到。
func (c *DashboardCache) Store(ctx context.Context, key string, d Dashboard) {
	if c == nil || c.rdb == nil || key == "" {
		return
	}
	raw, err := json.Marshal(d)
	if err != nil {
		c.log.Warn("report: 序列化仪表盘缓存失败", zap.Error(err))
		return
	}
	if err := c.rdb.Set(ctx, key, raw, dashboardTTL).Err(); err != nil {
		c.log.Warn("report: 写入仪表盘缓存失败", zap.Error(err))
	}
}

// Bump 在消费者成功提交后推进租户版本号，让该租户所有旧缓存键立即失效。
func (c *DashboardCache) Bump(ctx context.Context, tenantID uuid.UUID) {
	if c == nil || c.rdb == nil {
		return
	}
	if err := c.rdb.Incr(ctx, versionKey(tenantID)).Err(); err != nil {
		c.log.Warn("report: 推进仪表盘缓存版本失败，旧结果将在 TTL 后自然过期",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
	}
}
