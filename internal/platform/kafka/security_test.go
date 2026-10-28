package kafka

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
