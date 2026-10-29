package kafka

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurityFromConfigKeepsTLS 守住一次真实事故：四个 cmd 各自手拼
// Security 时集体漏了 TLS，本地（明文 Kafka）全部通过，切到托管 Kafka
// 后所有服务启动即报 "配置了 tls_ca_file 但没有开启 tls"。
func TestSecurityFromConfigKeepsTLS(t *testing.T) {
	got := SecurityFromConfig(config.Kafka{
		Brokers:       []string{"b:9095"},
		TLS:           true,
		TLSCAFile:     "/etc/expensio/kafka/ca.pem",
		SASLMechanism: "plain",
		SASLUsername:  "u",
		SASLPassword:  "p",
	})
	require.True(t, got.TLS, "TLS 必须在转换中保留")
	assert.Equal(t, "/etc/expensio/kafka/ca.pem", got.TLSCAFile)
	assert.Equal(t, "plain", got.SASLMechanism)
	assert.Equal(t, "u", got.SASLUsername)
	assert.Equal(t, "p", got.SASLPassword)

	// 明文集群仍然保持零值语义。
	assert.Equal(t, Security{}, SecurityFromConfig(config.Kafka{}))
}

func TestSecurityOptions(t *testing.T) {
	tests := []struct {
		name    string
		sec     Security
		want    int // 期望生成的 kgo.Opt 数量（TLS + SASL）
		wantErr string
	}{
		{name: "zero value is plaintext", sec: Security{}, want: 0},
		{name: "tls only", sec: Security{TLS: true}, want: 1},
		{
			name: "plain over tls",
			sec:  Security{TLS: true, SASLMechanism: "plain", SASLUsername: "u", SASLPassword: "p"},
			want: 2,
		},
		{
			name: "scram-sha-256 over tls",
			sec:  Security{TLS: true, SASLMechanism: "SCRAM-SHA-256", SASLUsername: "u", SASLPassword: "p"},
			want: 2,
		},
		{
			name: "scram-sha-512 over tls",
			sec:  Security{TLS: true, SASLMechanism: "scram-sha-512", SASLUsername: "u", SASLPassword: "p"},
			want: 2,
		},
		{
			name:    "plain without tls is rejected",
			sec:     Security{SASLMechanism: "plain", SASLUsername: "u", SASLPassword: "p"},
			wantErr: "必须配合 TLS",
		},
		{
			name:    "unknown mechanism",
			sec:     Security{TLS: true, SASLMechanism: "oauthbearer", SASLUsername: "u", SASLPassword: "p"},
			wantErr: "不支持",
		},
		{
			name:    "mechanism without credentials",
			sec:     Security{TLS: true, SASLMechanism: "plain"},
			wantErr: "用户名与口令",
		},
		{
			name:    "credentials without mechanism",
			sec:     Security{TLS: true, SASLUsername: "u", SASLPassword: "p"},
			wantErr: "没有 sasl_mechanism",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := tt.sec.options()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, opts, tt.want)
		})
	}
}

func TestNewProducerRejectsInvalidSecurityBeforeDialing(t *testing.T) {
	// 校验发生在 kgo.NewClient 之前：不会尝试连接 127.0.0.1。
	_, err := NewProducer([]string{"127.0.0.1:9092"}, "expense", Security{
		SASLMechanism: "plain",
		SASLUsername:  "u",
		SASLPassword:  "p",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "必须配合 TLS")

	_, err = NewProducer([]string{"127.0.0.1:9092"}, "expense", Security{}, Security{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "至多传一个")
}

func TestSecurityTLSCAFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	invalid := filepath.Join(dir, "invalid.pem")
	require.NoError(t, os.WriteFile(invalid, []byte("not a certificate"), 0o600))

	// CA 文件读不到要在建客户端之前失败，而不是等第一次请求才发现。
	_, err := Security{TLS: true, TLSCAFile: missing}.options()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "读取 tls_ca_file 失败")

	// 内容不是证书时同样拒绝：空根池会表现为"所有 broker 校验失败"，
	// 很难和网络问题区分。
	_, err = Security{TLS: true, TLSCAFile: invalid}.options()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可用的 PEM 证书")

	// 没开 TLS 却给 CA 文件，说明配置写错了。
	_, err = Security{TLSCAFile: invalid}.options()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有开启 tls")
}
