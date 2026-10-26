package config_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/config"
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
		Service:     "billing-service",
		DefaultAddr: ":8083",
		NeedsDB:     true,
		NeedsKafka:  true,
		NeedsJWT:    true,
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "EXPENSIO_DB_NAME")
	assert.Contains(t, msg, "EXPENSIO_JWT_SECRET")
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

	rendered := fmt.Sprintf("%v", *cfg)
	assert.NotContains(t, rendered, "super-secret-password")
	assert.NotContains(t, rendered, "super-secret-jwt")
	assert.Contains(t, rendered, "***")
}
