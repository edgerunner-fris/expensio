// Package objectstore 封装 MinIO / S3 兼容对象存储。
//
// 费用附件走浏览器直传：服务端只签发短期 presigned URL，并读取对象元数据，
// 不让文件字节经过 API 进程。这样上传带宽由对象存储承担，API 只做鉴权与记账。
package objectstore

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config 是对象存储连接参数。Endpoint 是 host:port，不带 scheme；
// 是否 TLS 由 UseSSL 决定。密钥只来自环境变量 / K8s Secret。
//
// PublicEndpoint 是浏览器看到的主机名，只用于签名 URL：
// SigV4 把 Host 算进签名，因此容器内地址（minio:9000）签出来的 URL
// 在浏览器里既解析不到、也会因 Host 不符被拒。留空表示与 Endpoint 相同。
type Config struct {
	Endpoint       string
	PublicEndpoint string
	AccessKey      string
	SecretKey      string
	Bucket         string
	UseSSL         bool
}

// Store 是 MinIO 客户端的最小封装。它不缓存对象，也不产生临时文件。
type Store struct {
	client *minio.Client
	// signer 只用于生成 presigned URL，可以是不可达的公网地址。
	signer *minio.Client
	bucket string
}

// New 建立客户端并确保桶存在。
//
// 桶不存在时自动创建：本地 Compose 与集群验收环境都靠这个行为保持一致，
// 生产也可以用只授予该桶权限的账号。MinIO 不可达时启动失败，而不是等到
// 第一次上传才发现存储没配好。
func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("objectstore: endpoint 不能为空")
	}
	if strings.Contains(cfg.Endpoint, "://") {
		return nil, fmt.Errorf("objectstore: endpoint 不能带 scheme，当前为 %q", cfg.Endpoint)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("objectstore: access key 与 secret key 不能为空")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("objectstore: bucket 不能为空")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: 初始化客户端失败: %w", err)
	}

	signer := client
	if pub := strings.TrimSpace(cfg.PublicEndpoint); pub != "" && pub != cfg.Endpoint {
		if strings.Contains(pub, "://") {
			return nil, fmt.Errorf("objectstore: public endpoint 不能带 scheme，当前为 %q", pub)
		}
		signer, err = minio.New(pub, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: cfg.UseSSL,
		})
		if err != nil {
			return nil, fmt.Errorf("objectstore: 初始化签名客户端失败: %w", err)
		}
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: 检查桶 %q 失败: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("objectstore: 创建桶 %q 失败: %w", cfg.Bucket, err)
		}
	}
	return &Store{client: client, signer: signer, bucket: cfg.Bucket}, nil
}

// PresignPut 签发只允许 PUT 的上传 URL。Content-Type 进入签名：
// 客户端必须带上同名头，否则签名校验失败。这样确认接口看到的元数据
// 就是上传时声明的那一份，而不是事后随便改的值。
func (s *Store) PresignPut(ctx context.Context, key string, expiry time.Duration, contentType string) (string, error) {
	headers := http.Header{"Content-Type": []string{contentType}}
	u, err := s.signer.PresignHeader(ctx, http.MethodPut, s.bucket, key, expiry, nil, headers)
	if err != nil {
		return "", fmt.Errorf("objectstore: 签发上传 URL 失败: %w", err)
	}
	return u.String(), nil
}

// PresignGet 签发下载 URL，并强制浏览器按附件下载。
// 文件名放在 response-content-disposition 里，避免对象键（UUID）暴露给用户。
func (s *Store) PresignGet(ctx context.Context, key string, expiry time.Duration, fileName string) (string, error) {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": fileName})
	if disposition == "" {
		disposition = "attachment"
	}
	u, err := s.signer.PresignedGetObject(ctx, s.bucket, key, expiry,
		url.Values{"response-content-disposition": []string{disposition}})
	if err != nil {
		return "", fmt.Errorf("objectstore: 签发下载 URL 失败: %w", err)
	}
	return u.String(), nil
}

// Stat 返回对象大小与内容类型。exists=false 表示对象不存在；
// 其他错误（网络、鉴权）才作为 error 返回。调用方不需要理解 S3 错误码。
func (s *Store) Stat(ctx context.Context, key string) (size int64, contentType string, exists bool, err error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	return info.Size, info.ContentType, true, nil
}

// Remove 删除对象。删除不存在的对象在 S3 语义下也算成功，
// 因此这里不额外区分。
func (s *Store) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
