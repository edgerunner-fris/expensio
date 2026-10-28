package testenv

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresTLS 是启用 TLS 的测试实例，服务端证书由一次性 CA 签发。
//
// 用途是验证 sslmode=verify-full + sslrootcert 的真实链路：证书链、主机名
// 与 CA 任何一处不匹配都必须握手失败。与明文实例一样，它执行仓库里的
// 真实初始化脚本与迁移，而不是另一套 DDL。
type PostgresTLS struct {
	Host string
	Port string
	// CAFile 是宿主上的 CA 证书路径，测试用它构造连接串。
	CAFile string

	dir       string
	container testcontainers.Container
}

var (
	pgTLSOnce     sync.Once
	pgTLSInstance *PostgresTLS
	pgTLSErr      error
)

// StartPostgresTLS 启动（或复用）TLS 测试实例，并按服务名执行迁移。
func StartPostgresTLS(t *testing.T, services ...string) *PostgresTLS {
	t.Helper()
	requireDocker(t)

	pgTLSOnce.Do(func() { pgTLSInstance, pgTLSErr = startPostgresTLS(services) })
	if pgTLSErr != nil {
		t.Fatalf("testenv: 启动 TLS PostgreSQL 失败: %v", pgTLSErr)
	}
	return pgTLSInstance
}

func startPostgresTLS(services []string) (*PostgresTLS, error) {
	initDir := filepath.Join(repoRoot(), "deploy", "local", "postgres", "init")
	if _, err := os.Stat(initDir); err != nil {
		return nil, fmt.Errorf("找不到数据库初始化脚本目录: %w", err)
	}

	dir, err := os.MkdirTemp("", "expensio-pg-tls-")
	if err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("调整证书目录权限失败: %w", err)
	}
	caFile, err := writeTestCertificates(dir)
	if err != nil {
		return nil, err
	}

	// 容器默认以 root 进入 entrypoint：先把挂载进来的证书复制到可控制权限的
	// 目录并交给 postgres 用户，再交给官方入口脚本。直接把 bind mount 的
	// 0600 文件指给服务端会因属主是宿主用户而被拒绝。
	script := `set -e
mkdir -p /etc/postgres-tls
cp /certs/ca.pem /certs/server.crt /certs/server.key /etc/postgres-tls/
chown postgres:postgres /etc/postgres-tls/ca.pem /etc/postgres-tls/server.crt /etc/postgres-tls/server.key
chmod 600 /etc/postgres-tls/server.key
exec /usr/local/bin/docker-entrypoint.sh postgres \
  -c ssl=on \
  -c ssl_cert_file=/etc/postgres-tls/server.crt \
  -c ssl_key_file=/etc/postgres-tls/server.key`

	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Entrypoint:   []string{"/bin/sh", "-c"},
			Cmd:          []string{script},
			Env: map[string]string{
				"POSTGRES_USER":                 superuser,
				"POSTGRES_PASSWORD":             superuserPassword,
				"POSTGRES_DB":                   "postgres",
				"EXPENSIO_DB_APP_PASSWORD":      appPassword,
				"EXPENSIO_DB_MIGRATOR_PASSWORD": migratorPassword,
			},
			Mounts: testcontainers.ContainerMounts{
				testcontainers.BindMount(dir, "/certs"),
				testcontainers.BindMount(initDir, "/docker-entrypoint-initdb.d"),
			},
			// 初始化会先起临时实例跑脚本再重启；只等一次就建连会被重启打断。
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeout(containerStartTime),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("启动 TLS postgres 容器失败: %w", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		_ = ctr.Terminate(context.Background())
		return nil, err
	}
	mapped, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		_ = ctr.Terminate(context.Background())
		return nil, err
	}

	pg := &PostgresTLS{
		Host:      host,
		Port:      mapped.Port(),
		CAFile:    caFile,
		dir:       dir,
		container: ctr,
	}
	for _, svc := range services {
		if err := migrateUp(svc, pg.migratorDSN(svc)); err != nil {
			_ = ctr.Terminate(context.Background())
			return nil, fmt.Errorf("迁移 %s 库失败: %w", svc, err)
		}
	}
	return pg, nil
}

// AppDSN 返回应用角色的 verify-full 连接串，CA 为本次实例的签发者。
func (p *PostgresTLS) AppDSN(service string) string {
	return p.appDSN(service, p.CAFile)
}

// AppDSNWithCA 允许测试替换 CA，用来验证"信任了错误的 CA 必须失败"。
func (p *PostgresTLS) AppDSNWithCA(service, caFile string) string {
	return p.appDSN(service, caFile)
}

func (p *PostgresTLS) appDSN(service, caFile string) string {
	return p.dsn(service, service+"_app", appPassword, "postgres", caFile)
}

func (p *PostgresTLS) migratorDSN(service string) string {
	return p.dsn(service, service+"_migrator", migratorPassword, "pgx5", p.CAFile)
}

func (p *PostgresTLS) dsn(service, user, password, scheme, caFile string) string {
	u := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(p.Host, p.Port),
		Path:   "/" + service + "_db",
	}
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", caFile)
	u.RawQuery = q.Encode()
	return u.String()
}

// cleanupPostgresTLS 由 Cleanup 调用。
func (p *PostgresTLS) cleanup(ctx context.Context) {
	_ = p.container.Terminate(ctx)
	_ = os.RemoveAll(p.dir)
}
