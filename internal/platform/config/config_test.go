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

func TestMissingRequiredDBNameIsReported(t *testing.T) {
	_, err := config.Load(config.Options{
		Service:     "identity-service",
		DefaultAddr: ":8081",
		NeedsDB:     true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_DB_NAME")
}

func TestShortJWTSecretIsRejected(t *testing.T) {
	t.Setenv("EXPENSIO_JWT_SECRET", "too-short")

	_, err := config.Load(config.Options{
		Service:     "gateway",
		DefaultAddr: ":8080",
		NeedsJWT:    true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EXPENSIO_JWT_SECRET")
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
	assert.Contains(t, msg, "EXPENSIO_JWT_SECRET")
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
	cfg.JWT.Secret = "super-secret-jwt"
	cfg.Billing.WebhookSecret = "super-secret-webhook"
	cfg.Notify.InternalToken = "super-secret-internal"
	cfg.Notify.SMTPPassword = "super-secret-smtp"

	rendered := fmt.Sprintf("%v", *cfg)
	assert.NotContains(t, rendered, "super-secret-password")
	assert.NotContains(t, rendered, "super-secret-jwt")
	assert.NotContains(t, rendered, "super-secret-webhook")
	assert.NotContains(t, rendered, "super-secret-internal")
	assert.NotContains(t, rendered, "super-secret-smtp")
	assert.Contains(t, rendered, "***")
}

func TestIdentityRequiresEd25519PrivateKey(t *testing.T) {
	// 即使有合法的 HS256 密钥，签发方也必须配置私钥：否则它会退回 HMAC 签名，
	// 迁移永远无法完成。
	t.Setenv("EXPENSIO_JWT_SECRET", strings.Repeat("s", 40))
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
	t.Setenv("EXPENSIO_JWT_ALLOW_HS256", "false")

	cfg, err := config.Load(config.Options{
		Service:     "gateway",
		DefaultAddr: ":8080",
		NeedsJWT:    true,
	})
	require.NoError(t, err, "不接受 HS256、没有共享密钥，只靠公钥也应能启动")

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

func TestShortHS256SecretStillRejectedWhenEnabled(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("EXPENSIO_JWT_PUBLIC_KEYS", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("EXPENSIO_JWT_SECRET", "short")
	t.Setenv("EXPENSIO_JWT_ALLOW_HS256", "true")

	_, err = config.Load(config.Options{Service: "gateway", DefaultAddr: ":8080", NeedsJWT: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwt.secret")
}
