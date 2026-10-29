package kafka

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Security 是 broker 连接的安全参数。零值表示明文、无认证，与本地 compose
// 的 Kafka 一致；托管 Kafka 通过环境变量注入 TLS + SASL。
type Security struct {
	TLS bool
	// TLSCAFile 是额外根证书 PEM。为空时用系统根证书；私有 CA 才需要。
	TLSCAFile     string
	SASLMechanism string
	SASLUsername  string
	SASLPassword  string
}

// SecurityFromConfig 把配置层的 Kafka 参数翻译成连接参数。
// 四个服务的 cmd 必须走这里：字段散在各处手写时漏掉一个 TLS，本地
// （TLS=false）全绿，只在托管 Kafka 上启动即失败。这不是假想——2026-10-30
// 切 ApsaraMQ 时四个服务都漏了 TLS，Pod 全部 CrashLoop。
func SecurityFromConfig(c config.Kafka) Security {
	return Security{
		TLS:           c.TLS,
		TLSCAFile:     c.TLSCAFile,
		SASLMechanism: c.SASLMechanism,
		SASLUsername:  c.SASLUsername,
		SASLPassword:  c.SASLPassword,
	}
}

// tlsConfig 构造客户端 TLS 配置。只有显式给了 CA 文件才覆盖 RootCAs，
// 否则保留系统根证书 —— 托管服务的证书都签自公有 CA，塞一个空池会全部失败。
func (s Security) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLSCAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(s.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("kafka: 读取 tls_ca_file 失败: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// SystemCertPool 在精简容器里可能返回错误；私有 CA 场景下
		// 用空池也能工作，不要因此拒绝启动。
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("kafka: tls_ca_file %s 里没有可用的 PEM 证书", s.TLSCAFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// options 把 Security 翻译成 franz-go 选项，并在构造客户端之前校验。
// 校验放在这里而不是只放在配置层：测试和其他调用方（集成测试、工具）
// 也可能直接构造客户端，不能依赖"配置层已经校验过"。
func (s Security) options() ([]kgo.Opt, error) {
	mech := strings.ToLower(strings.TrimSpace(s.SASLMechanism))
	var opts []kgo.Opt

	if s.TLS {
		cfg, err := s.tlsConfig()
		if err != nil {
			return nil, err
		}
		opts = append(opts, kgo.DialTLSConfig(cfg))
	} else if s.TLSCAFile != "" {
		return nil, errors.New("kafka: 配置了 tls_ca_file 但没有开启 tls")
	}

	if mech == "" {
		if s.SASLUsername != "" || s.SASLPassword != "" {
			return nil, errors.New("kafka: 配置了 SASL 用户名/口令但没有 sasl_mechanism")
		}
		return opts, nil
	}
	if s.SASLUsername == "" || s.SASLPassword == "" {
		return nil, errors.New("kafka: sasl_mechanism 需要同时提供用户名与口令")
	}
	if !s.TLS {
		// SASL 保护认证，不保护数据；托管服务的 SASL 端点都要求 TLS。
		// 明确拒绝，避免口令与事件明文过网。
		return nil, errors.New("kafka: SASL 认证必须配合 TLS")
	}

	switch mech {
	case "plain":
		opts = append(opts, kgo.SASL(plain.Auth{
			User: s.SASLUsername,
			Pass: s.SASLPassword,
		}.AsMechanism()))
	case "scram-sha-256":
		opts = append(opts, kgo.SASL(scram.Auth{
			User: s.SASLUsername,
			Pass: s.SASLPassword,
		}.AsSha256Mechanism()))
	case "scram-sha-512":
		opts = append(opts, kgo.SASL(scram.Auth{
			User: s.SASLUsername,
			Pass: s.SASLPassword,
		}.AsSha512Mechanism()))
	default:
		return nil, fmt.Errorf("kafka: 不支持的 sasl_mechanism %q（可选 plain / scram-sha-256 / scram-sha-512）", s.SASLMechanism)
	}
	return opts, nil
}
