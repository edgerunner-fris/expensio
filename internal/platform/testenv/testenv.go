// Package testenv 为集成测试提供真实依赖：PostgreSQL 与 Redis。
//
// 只被 *_test.go 引用，任何服务的二进制都不会链接它。
//
// 刻意复用仓库里的真实初始化脚本（deploy/local/postgres/init/01-bootstrap.sh）与
// 真实迁移文件，而不是在测试里另写一套建库 SQL：
// 测出来的东西必须和生产走的是同一份 DDL，否则"测试过了、线上 RLS 没开"依然可能发生。
package testenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

const (
	// 测试容器的口令只在容器生命周期内有效，随容器一起消失；
	// 生产配置永远来自环境变量 / K8s Secret，见 README。
	superuserPassword  = "test_superuser_pw"
	appPassword        = "test_app_pw"
	migratorPassword   = "test_migrator_pw"
	superuser          = "expensio"
	postgresImage      = "postgres:16.15-alpine"
	redisImage         = "redis:7.4.11-alpine"
	containerStartTime = 3 * time.Minute
)

// Postgres 是已建库并完成迁移的测试实例。
type Postgres struct {
	Host string
	Port string
	// 容器句柄交给 ClosePostgres 释放，不对外暴露。
	container *postgres.PostgresContainer
}

var (
	pgOnce     sync.Once
	pgInstance *Postgres
	pgErr      error

	redisOnce     sync.Once
	redisAddr     string
	redisInstance *redis.RedisContainer
	redisErr      error

	dockerOnce sync.Once
	dockerErr  error
)

func init() {
	// 容器由 Cleanup 显式 Terminate，不依赖 Ryuk 这个"兜底回收器"。
	// Ryuk 需要额外拉取 testcontainers/ryuk 镜像；在离线或代理不通的环境里，
	// 它会先于测试本身失败，把"没有 Docker 网络"伪装成"集成测试挂了"。
	// 想重新启用就显式设置 TESTCONTAINERS_RYUK_DISABLED=false。
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}
}

// requireDocker 检查 Docker 是否可用。
//
// 本地没装 Docker 时允许跳过；但只要设置了 EXPENSIO_REQUIRE_DOCKER=1
// （CI 固定设置），不可用就必须失败——"跳过"绝不能被当成"通过"。
// 用 docker info 而不是 testcontainers 的健康检查，是为了让失败原因
// 直接指向 daemon 或权限，而不是容器运行时的内部状态。
func requireDocker(t *testing.T) {
	t.Helper()
	dockerOnce.Do(func() {
		dockerErr = exec.Command("docker", "info").Run()
	})
	if dockerErr == nil {
		return
	}
	if os.Getenv("EXPENSIO_REQUIRE_DOCKER") == "1" {
		t.Fatalf("集成测试要求 Docker 可用，但 docker info 失败: %v", dockerErr)
	}
	t.Skipf("Docker 不可用，跳过依赖容器的集成测试: %v", dockerErr)
}

// StartPostgres 启动（或复用）测试用 PostgreSQL，并按服务名执行迁移。
//
// 首次调用决定迁移哪些库；后续调用直接复用实例。
func StartPostgres(t *testing.T, services ...string) *Postgres {
	t.Helper()
	requireDocker(t)

	pgOnce.Do(func() { pgInstance, pgErr = startPostgres(services) })
	if pgErr != nil {
		t.Fatalf("testenv: 启动 PostgreSQL 失败: %v", pgErr)
	}
	return pgInstance
}

func startPostgres(services []string) (*Postgres, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	initScript := filepath.Join(repoRoot(), "deploy", "local", "postgres", "init", "01-bootstrap.sh")
	if _, err := os.Stat(initScript); err != nil {
		return nil, fmt.Errorf("找不到数据库初始化脚本: %w", err)
	}

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername(superuser),
		postgres.WithPassword(superuserPassword),
		// 用真实初始化脚本建五个库与两套角色（migrator / app NOBYPASSRLS）。
		postgres.WithInitScripts(initScript),
		// 必须显式等待：postgres 镜像首次启动会先起一个临时实例跑初始化脚本，
		// 然后重启一次。只等"端口已监听"会在临时实例上建立连接，
		// 随后被重启打断——表现为迁移时报 unexpected EOF。
		postgres.BasicWaitStrategies(),
		testcontainers.WithEnv(map[string]string{
			"EXPENSIO_DB_APP_PASSWORD":      appPassword,
			"EXPENSIO_DB_MIGRATOR_PASSWORD": migratorPassword,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("启动 postgres 容器失败: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		return nil, err
	}
	mapped, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}

	pg := &Postgres{Host: host, Port: mapped.Port(), container: container}
	for _, svc := range services {
		if err := migrateUp(svc, pg.dsn(svc, svc+"_migrator", migratorPassword, "pgx5")); err != nil {
			return nil, fmt.Errorf("迁移 %s 库失败: %w", svc, err)
		}
	}
	return pg, nil
}

// AppDSN 返回应用角色（NOBYPASSRLS）的连接串。测试必须用它，否则 RLS 不生效。
func (p *Postgres) AppDSN(service string) string {
	return p.dsn(service, service+"_app", appPassword, "postgres")
}

// MigratorDSN 返回库属主角色的连接串，只用于断言表结构。
func (p *Postgres) MigratorDSN(service string) string {
	return p.dsn(service, service+"_migrator", migratorPassword, "postgres")
}

func (p *Postgres) dsn(service, user, password, scheme string) string {
	u := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(p.Host, p.Port),
		Path:   "/" + service + "_db",
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

// MigrateDownUp 将某个服务的迁移回滚 steps 步，再重新执行到最新。
// 仅用于验证 down SQL；测试共享同一个实例，调用后该库的表会被重建。
func (p *Postgres) MigrateDownUp(t *testing.T, service string, steps int) {
	t.Helper()
	dir := filepath.Join(repoRoot(), "migrations", service)
	src, err := iofs.New(os.DirFS(dir), ".")
	if err != nil {
		t.Fatalf("读取迁移目录 %s 失败: %v", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src,
		p.dsn(service, service+"_migrator", migratorPassword, "pgx5"))
	if err != nil {
		t.Fatalf("初始化迁移失败: %v", err)
	}
	if err := m.Steps(-steps); err != nil {
		_, _ = m.Close()
		t.Fatalf("回滚 %d 步失败: %v", steps, err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		_, _ = m.Close()
		t.Fatalf("重新迁移失败: %v", err)
	}
	srcErr, dbErr := m.Close()
	if srcErr != nil || dbErr != nil {
		t.Fatalf("关闭迁移失败: %v / %v", srcErr, dbErr)
	}
}

func migrateUp(service, dsn string) error {
	dir := filepath.Join(repoRoot(), "migrations", service)
	src, err := iofs.New(os.DirFS(dir), ".")
	if err != nil {
		return fmt.Errorf("读取迁移目录 %s 失败: %w", dir, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		_, _ = m.Close()
		return err
	}
	srcErr, dbErr := m.Close()
	return errors.Join(srcErr, dbErr)
}

// StartRedis 启动（或复用）测试用 Redis，返回 host:port。
func StartRedis(t *testing.T) string {
	t.Helper()
	requireDocker(t)

	redisOnce.Do(func() { redisInstance, redisAddr, redisErr = startRedis() })
	if redisErr != nil {
		t.Fatalf("testenv: 启动 Redis 失败: %v", redisErr)
	}
	return redisAddr
}

func startRedis() (*redis.RedisContainer, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	container, err := redis.Run(ctx, redisImage)
	if err != nil {
		return nil, "", fmt.Errorf("启动 redis 容器失败: %w", err)
	}
	host, err := container.Host(ctx)
	if err != nil {
		return nil, "", err
	}
	mapped, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return nil, "", err
	}
	return container, net.JoinHostPort(host, mapped.Port()), nil
}

// Cleanup 停止本包启动的容器。由各测试包的 TestMain 在 m.Run() 之后调用。
// Ryuk 默认关闭（见 init），所以这里必须被调用；测试进程被强杀时仍可能残留容器。
func Cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if redisInstance != nil {
		_ = redisInstance.Terminate(ctx)
	}
	if pgInstance != nil {
		_ = pgInstance.container.Terminate(ctx)
	}
}

// repoRoot 从本文件位置回溯到仓库根目录（internal/platform/testenv → ../../..）。
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testenv: 无法定位源码目录")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}
