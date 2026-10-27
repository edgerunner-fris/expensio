// Package db 是服务访问 PostgreSQL 的唯一入口。
//
// 刻意不暴露 *pgxpool.Pool：一旦业务代码能拿到裸池，就会有人绕过事务直接查表，
// 租户上下文随之丢失，RLS 变成摆设。对外只有两个方法：
//   - WithinTenant：建立租户上下文后执行，受 RLS 约束；
//   - WithTx：登录、刷新这类"还不知道租户是谁"的操作，只能碰无 RLS 的表。
//
// 上下文用 set_config(..., true) 设在事务内：事务结束自动失效，
// 连接归还池里不会把上一个请求的租户带给下一个请求。
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

// DB 包住连接池，只暴露事务级访问。
type DB struct {
	pool *pgxpool.Pool
}

// Open 建立连接池并做一次连通性检查：启动阶段失败比第一个请求失败容易排查。
func Open(ctx context.Context, dsn string, maxConns int32) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: 解析 DSN 失败: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: 建立连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: 连接数据库失败: %w", err)
	}
	d := &DB{pool: pool}
	// 指标注册失败不应阻止提供服务；错误在 RegisterMetrics 内部已隔离。
	_ = d.RegisterMetrics()
	return d, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

// Ping 供 readiness 探针使用。
func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

// WithTx 用于租户上下文建立之前必须完成的查询（按 slug 找租户、按邮箱找用户、
// 按摘要找刷新令牌）。调用方只能访问无 RLS 的表，这一点由表设计保证。
func (d *DB) WithTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return d.inTx(ctx, fn, uuid.Nil)
}

// WithinTenant 在事务内设置 app.tenant_id 后执行 fn。
// 缺少租户上下文时直接失败，而不是退回全表可读。
func (d *DB) WithinTenant(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	t, ok := tenantctx.From(ctx)
	if !ok {
		return apperr.Internal(errors.New("db: 未设置 tenantctx"))
	}
	return d.inTx(ctx, fn, t.ID)
}

// WithinTenantID 用于"凭据本身已经证明租户归属"的内部事务：
// 登录成功后按查到的租户建立上下文、刷新令牌按令牌记录里的 tenant_id 建立上下文。
//
// 它与 WithinTenant 的区别只在于租户 ID 的来源，因此调用点必须满足：
// tenantID 来自数据库里的可信记录，绝不是请求参数。任何从 body / query / path
// 取出 tenant_id 再传给本方法，都会直接击穿 RLS 隔离。
func (d *DB) WithinTenantID(ctx context.Context, tenantID uuid.UUID, fn func(context.Context, pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return apperr.Internal(errors.New("db: 租户 ID 不能为空"))
	}
	return d.inTx(ctx, fn, tenantID)
}

func (d *DB) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error, tenantID uuid.UUID) error {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("db: 开启事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxClosed，忽略即可。
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if tenantID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return fmt.Errorf("db: 设置租户上下文失败: %w", err)
		}
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: 提交事务失败: %w", err)
	}
	return nil
}
