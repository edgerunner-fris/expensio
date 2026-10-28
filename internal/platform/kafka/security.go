package kafka

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Security 是 broker 连接的安全参数。零值表示明文、无认证，与本地 compose
// 的 Kafka 一致；托管 Kafka 通过环境变量注入 TLS + SASL。
type Security struct {
	TLS           bool
	SASLMechanism string
	SASLUsername  string
	SASLPassword  string
}

// options 把 Security 翻译成 franz-go 选项，并在构造客户端之前校验。
// 校验放在这里而不是只放在配置层：测试和其他调用方（集成测试、工具）
// 也可能直接构造客户端，不能依赖"配置层已经校验过"。
func (s Security) options() ([]kgo.Opt, error) {
	mech := strings.ToLower(strings.TrimSpace(s.SASLMechanism))
	var opts []kgo.Opt

	if s.TLS {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
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
