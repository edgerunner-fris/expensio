package testenv

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// Docker Hub 官方 minio 镜像已下架，改用 Bitnami 的归档镜像并固定 digest，
	// 避免同一个 tag 在不同时间拉到不同内容。
	minioImage      = "bitnamilegacy/minio@sha256:451fe6858cb770cc9d0e77ba811ce287420f781c7c1b806a386f6896471a349c"
	minioPort       = "9000/tcp"
	minioAccessKey  = "test_minio_access"
	minioSecretKey  = "test_minio_secret_pw"
	minioHealthPath = "/minio/health/live"
	minioTestBucket = "expensio-test"
)

// MinIO 是已启动的测试对象存储。Endpoint 不带 scheme，供 objectstore.New 使用。
type MinIO struct {
	Host      string
	Port      string
	AccessKey string
	SecretKey string
	Bucket    string
	container testcontainers.Container
}

// Endpoint 返回 host:port。
func (m *MinIO) Endpoint() string { return net.JoinHostPort(m.Host, m.Port) }

// StartMinIO 启动（或复用）测试用 MinIO，供附件相关集成测试使用。
func StartMinIO(t *testing.T) *MinIO {
	t.Helper()
	requireDocker(t)

	minioOnce.Do(func() { minioInstance, minioErr = startMinIO() })
	if minioErr != nil {
		t.Fatalf("testenv: 启动 MinIO 失败: %v", minioErr)
	}
	return minioInstance
}

func startMinIO() (*MinIO, error) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			ExposedPorts: []string{minioPort},
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioAccessKey,
				"MINIO_ROOT_PASSWORD": minioSecretKey,
			},
			// 健康端点返回 200 才说明数据目录已经就绪；只看端口会让
			// 第一次 BucketExists 撞上"服务已监听但未就绪"。
			WaitingFor: wait.ForHTTP(minioHealthPath).
				WithPort(minioPort).
				WithStartupTimeout(containerStartTime),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("启动 minio 容器失败: %w", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, err
	}
	mapped, err := ctr.MappedPort(ctx, minioPort)
	if err != nil {
		return nil, err
	}
	return &MinIO{
		Host:      host,
		Port:      mapped.Port(),
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		Bucket:    minioTestBucket,
		container: ctr,
	}, nil
}
