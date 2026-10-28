package config_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/config"
	"github.com/edgerunner-fris/expensio/internal/platform/tenantctx"
)

func gatewayOptions() config.Options {
	return config.Options{Service: "gateway", DefaultAddr: ":8080"}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(gatewayOptions())
	require.NoError(t, err)

	assert.Equal(t, "gateway", cfg.Service)
	assert.Equal(t, "local", cfg.Env)
	assert.Equal(t, ":8080", cfg.HTTP.Addr)
	assert.Equal(t, 5*time.Second, cfg.HTTP.ReadTimeout)
	assert.Equal(t, 20*time.Second, cfg.HTTP.ShutdownTimeout)
	assert.Equal(t, "expensio", cfg.JWT.Issuer)
}

func TestEnvOverrideWins(t *testing.T) {
	t.Setenv("EXPENSIO_HTTP_ADDR", ":9999")
	t.Setenv("EXPENSIO_LOG_LEVEL", "debug")

	cfg, err := config.Load(gatewayOptions())
	require.NoError(t, err)

	assert.Equal(t, ":9999", cfg.HTTP.Addr)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestKafkaBrokersSplitFromEnv(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_BROKERS", "k1:9092,k2:9092")

	cfg, err := config.Load(config.Options{
		Service:     "report-service",
		DefaultAddr: ":8084",
		NeedsKafka:  true,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"k1:9092", "k2:9092"}, cfg.Kafka.Brokers)
}

func kafkaOptions() config.Options {
	return config.Options{Service: "report-service", DefaultAddr: ":8084", NeedsKafka: true}
}

func TestKafkaSASLRequiresTLS(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_SASL_MECHANISM", "plain")
	t.Setenv("EXPENSIO_KAFKA_SASL_USERNAME", "u")
	t.Setenv("EXPENSIO_KAFKA_SASL_PASSWORD", "p")

	_, err := config.Load(kafkaOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kafka.tls")
}

func TestKafkaSASLAcceptedWithTLS(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS", "true")
	t.Setenv("EXPENSIO_KAFKA_SASL_MECHANISM", "SCRAM-SHA-512")
	t.Setenv("EXPENSIO_KAFKA_SASL_USERNAME", "u")
	t.Setenv("EXPENSIO_KAFKA_SASL_PASSWORD", "p")

	cfg, err := config.Load(kafkaOptions())
	require.NoError(t, err)
	assert.True(t, cfg.Kafka.TLS)
	assert.Equal(t, "SCRAM-SHA-512", cfg.Kafka.SASLMechanism)
	assert.Equal(t, "u", cfg.Kafka.SASLUsername)
}

func TestKafkaRejectsUnknownSASLMechanism(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS", "true")
	t.Setenv("EXPENSIO_KAFKA_SASL_MECHANISM", "oauthbearer")
	t.Setenv("EXPENSIO_KAFKA_SASL_USERNAME", "u")
	t.Setenv("EXPENSIO_KAFKA_SASL_PASSWORD", "p")

	_, err := config.Load(kafkaOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sasl_mechanism 非法")
}

func TestKafkaCredentialsWithoutMechanismAreRejected(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS", "true")
	t.Setenv("EXPENSIO_KAFKA_SASL_USERNAME", "u")
	t.Setenv("EXPENSIO_KAFKA_SASL_PASSWORD", "p")

	_, err := config.Load(kafkaOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sasl_mechanism 为空")
}

func TestKafkaSASLRequiresBothCredentials(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS", "true")
	t.Setenv("EXPENSIO_KAFKA_SASL_MECHANISM", "plain")
	t.Setenv("EXPENSIO_KAFKA_SASL_USERNAME", "u")

	_, err := config.Load(kafkaOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sasl_username 与 sasl_password")
}

func TestKafkaTLSCAFileRequiresTLS(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS_CA_FILE", "/etc/expensio/kafka/ca.pem")

	_, err := config.Load(kafkaOptions())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls_ca_file")
}

func TestKafkaTLSCAFileAcceptedWithTLS(t *testing.T) {
	t.Setenv("EXPENSIO_KAFKA_TLS", "true")
	t.Setenv("EXPENSIO_KAFKA_TLS_CA_FILE", "/etc/expensio/kafka/ca.pem")

	cfg, err := config.Load(kafkaOptions())
	require.NoError(t, err)
	assert.Equal(t, "/etc/expensio/kafka/ca.pem", cfg.Kafka.TLSCAFile)
}

func TestDBRootCertRequiresVerifyingSSLMode(t *testing.T) {
	t.Setenv("EXPENSIO_DB_NAME", "expense_db")
	t.Setenv("EXPENSIO_DB_SSL_ROOT_CERT", "/etc/expensio/postgres/ca.pem")

	_, err := config.Load(config.Options{
		Service: "expense", DefaultAddr: ":8082", NeedsDB: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ssl_root_cert")
}

func TestDBRootCertAcceptedWithVerifyFull(t *testing.T) {
	t.Setenv("EXPENSIO_DB_NAME", "expense_db")
	t.Setenv("EXPENSIO_DB_SSL_ROOT_CERT", "/etc/expensio/postgres/ca.pem")
	t.Setenv("EXPENSIO_DB_SSLMODE", "verify-full")

	cfg, err := config.Load(config.Options{
		Service: "expense", DefaultAddr: ":8082", NeedsDB: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "/etc/expensio/postgres/ca.pem", cfg.DB.SSLRootCert)
	assert.Contains(t, cfg.DB.DSN(), "sslrootcert=")
	assert.Contains(t, cfg.DB.DSN(), "sslmode=verify-full")
}

func TestDBRootCertRejectedTogetherWithDSNOverride(t *testing.T) {
	t.Setenv("EXPENSIO_DB_NAME", "expense_db")
	t.Setenv("EXPENSIO_DB_SSL_ROOT_CERT", "/etc/expensio/postgres/ca.pem")
	t.Setenv("EXPENSIO_DB_SSLMODE", "verify-full")
	t.Setenv("EXPENSIO_DB_DSN", "postgres://custom")

	_, err := config.Load(config.Options{
		Service: "expense", DefaultAddr: ":8082", NeedsDB: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db.dsn")
}

func TestDSNIncludesRootCert(t *testing.T) {
	d := config.DB{
		Host: "pg.internal", Port: 5432, User: "app", Password: "p",
		Name: "expense_db", SSLMode: "verify-full",
		SSLRootCert: "/etc/expensio/postgres/ca.pem",
	}

	assert.Contains(t, d.DSN(), "sslrootcert=%2Fetc%2Fexpensio%2Fpostgres%2Fca.pem")
}

func TestMissingRequiredDBNameIsReported(t *testing.T) {
	_, err := config.Load(config.Options{
		Service:     "identity-service",
		DefaultAddr: ":8081",
		NeedsDB:     true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_DB_NAME")
}

func TestMissingEd25519KeyMaterialIsRejected(t *testing.T) {
	// HS256 共享密钥已移除：只设置 EXPENSIO_JWT_SECRET 不再能启动验签服务。
	t.Setenv("EXPENSIO_JWT_SECRET", strings.Repeat("s", 40))

	_, err := config.Load(config.Options{
		Service:     "gateway",
		DefaultAddr: ":8080",
		NeedsJWT:    true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_JWT_PUBLIC_KEYS")
}

func TestValidationAggregatesAllProblems(t *testing.T) {
	_, err := config.Load(config.Options{
		Service:             "billing-service",
		DefaultAddr:         ":8083",
		NeedsDB:             true,
		NeedsKafka:          true,
		NeedsJWT:            true,
		NeedsBillingWebhook: true,
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "EXPENSIO_DB_NAME")
	assert.Contains(t, msg, "EXPENSIO_JWT_PUBLIC_KEYS")
	assert.Contains(t, msg, "EXPENSIO_BILLING_WEBHOOK_SECRET")
}

func TestShortBillingWebhookSecretIsRejected(t *testing.T) {
	t.Setenv("EXPENSIO_BILLING_WEBHOOK_SECRET", "too-short")

	_, err := config.Load(config.Options{
		Service:             "billing",
		DefaultAddr:         ":8083",
		NeedsBillingWebhook: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_BILLING_WEBHOOK_SECRET")
}

func TestInternalTokenIsRequiredForIdentityAndNotification(t *testing.T) {
	for _, opts := range []config.Options{
		{Service: "identity", DefaultAddr: ":8081", NeedsInternalAPI: true},
		{Service: "notification", DefaultAddr: ":8085", NeedsNotification: true},
	} {
		t.Run(opts.Service, func(t *testing.T) {
			_, err := config.Load(opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "EXPENSIO_NOTIFICATION_INTERNAL_TOKEN")
		})
	}
}

func TestNotificationRejectsInvalidSMTPAndIdentityURL(t *testing.T) {
	t.Setenv("EXPENSIO_NOTIFICATION_INTERNAL_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("EXPENSIO_NOTIFICATION_IDENTITY_URL", "not-a-url")
	t.Setenv("EXPENSIO_NOTIFICATION_SMTP_ADDR", "localhost")
	t.Setenv("EXPENSIO_NOTIFICATION_SMTP_FROM", "not-an-email")

	_, err := config.Load(config.Options{
		Service:           "notification",
		DefaultAddr:       ":8085",
		NeedsNotification: true,
	})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "notification.identity_url")
	assert.Contains(t, msg, "notification.smtp_addr")
	assert.Contains(t, msg, "notification.smtp_from")
}

func TestNotificationSMTPChecksCanBeDisabled(t *testing.T) {
	t.Setenv("EXPENSIO_NOTIFICATION_INTERNAL_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("EXPENSIO_NOTIFICATION_EMAIL_ENABLED", "false")
	t.Setenv("EXPENSIO_NOTIFICATION_SMTP_ADDR", "")
	t.Setenv("EXPENSIO_NOTIFICATION_SMTP_FROM", "")

	cfg, err := config.Load(config.Options{
		Service:           "notification",
		DefaultAddr:       ":8085",
		NeedsNotification: true,
	})
	require.NoError(t, err)
	assert.False(t, cfg.Notify.EmailEnabled)
}

func TestDSNEscapesSpecialCharacters(t *testing.T) {
	d := config.DB{
		Host: "localhost", Port: 5432,
		User: "app", Password: "p@ss/word", Name: "expense_db", SSLMode: "disable",
	}

	assert.Contains(t, d.DSN(), "p%40ss%2Fword")
	assert.Contains(t, d.DSN(), "expense_db")
}

func TestDSNOverridePassesThrough(t *testing.T) {
	d := config.DB{DSNOverride: "postgres://custom"}

	assert.Equal(t, "postgres://custom", d.DSN())
}

func TestStringRedactsSecrets(t *testing.T) {
	cfg, err := config.Load(config.Options{
		Service:     "gateway",
		DefaultAddr: ":8080",
		NeedsJWT:    false,
	})
	require.NoError(t, err)
	cfg.DB.Password = "super-secret-password"
	cfg.Billing.WebhookSecret = "super-secret-webhook"
	cfg.Notify.InternalToken = "super-secret-internal"
	cfg.Notify.SMTPPassword = "super-secret-smtp"

	rendered := fmt.Sprintf("%v", *cfg)
	assert.NotContains(t, rendered, "super-secret-password")
	assert.NotContains(t, rendered, "super-secret-webhook")
	assert.NotContains(t, rendered, "super-secret-internal")
	assert.NotContains(t, rendered, "super-secret-smtp")
	assert.Contains(t, rendered, "***")
}

func TestIdentityRequiresEd25519PrivateKey(t *testing.T) {
	// 签发方必须配置私钥；共享密钥已从配置结构中移除，没有回退路径。
	_, err := config.Load(config.Options{
		Service:        "identity",
		DefaultAddr:    ":8081",
		NeedsJWT:       true,
		NeedsJWTSigner: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_JWT_PRIVATE_KEY")
}

func TestEd25519PublicKeysSatisfyJWTValidationWithoutSecret(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("EXPENSIO_JWT_PUBLIC_KEYS", base64.StdEncoding.EncodeToString(pub))

	cfg, err := config.Load(config.Options{
		Service:     "gateway",
		DefaultAddr: ":8080",
		NeedsJWT:    true,
	})
	require.NoError(t, err, "没有共享密钥，只靠公钥也应能启动")

	verifier, err := cfg.JWT.AuthIssuer()
	require.NoError(t, err)
	_, _, err = verifier.IssueAccess(uuid.New(), uuid.New(), tenantctx.RoleMember, uuid.New())
	assert.Error(t, err, "只有公钥的服务不应能签发令牌")

	// 用私钥签出的令牌必须能被公钥验签方接受。
	signer, err := config.JWT{
		Issuer:     "expensio",
		PrivateKey: base64.StdEncoding.EncodeToString(priv.Seed()),
		AccessTTL:  cfg.JWT.AccessTTL,
		RefreshTTL: cfg.JWT.RefreshTTL,
	}.AuthIssuer()
	require.NoError(t, err)
	raw, _, err := signer.IssueAccess(uuid.New(), uuid.New(), tenantctx.RoleMember, uuid.New())
	require.NoError(t, err)
	_, err = verifier.ParseAccess(raw)
	require.NoError(t, err)
}

func TestTrustedProxyListParsesAndDeduplicates(t *testing.T) {
	t.Setenv("EXPENSIO_HTTP_TRUSTED_PROXIES", "10.0.0.0/8, 172.18.0.5,10.0.0.0/8")

	cfg, err := config.Load(gatewayOptions())
	require.NoError(t, err)
	list, err := cfg.HTTP.TrustedProxyList()
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "172.18.0.5"}, list)
}

func TestTrustedProxyListRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "0.0.0.0/0", want: "0 位前缀"},
		{value: "::/0", want: "0 位前缀"},
		{value: "not-an-ip", want: "不是合法 IP 或 CIDR"},
		{value: "10.0.0.0/8,,192.168.0.0/16", want: "含空项"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("EXPENSIO_HTTP_TRUSTED_PROXIES", tc.value)
			_, err := config.Load(gatewayOptions())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestIdentityWithPrivateKeyDoesNotNeedPublicKeys(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("EXPENSIO_JWT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))

	cfg, err := config.Load(config.Options{
		Service:        "identity",
		DefaultAddr:    ":8081",
		NeedsJWT:       true,
		NeedsJWTSigner: true,
	})
	require.NoError(t, err)

	issuer, err := cfg.JWT.AuthIssuer()
	require.NoError(t, err)
	raw, _, err := issuer.IssueAccess(uuid.New(), uuid.New(), tenantctx.RoleOwner, uuid.New())
	require.NoError(t, err)
	_, err = issuer.ParseAccess(raw)
	require.NoError(t, err)
}
