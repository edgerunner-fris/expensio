package db_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/db"
	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// TestMain 负责回收本包启动的 TLS PostgreSQL 容器。
func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}

// TestOpenVerifyFullWithPrivateCA 验证生产要用的那条链路：
// 自定义 CA + verify-full 能建立连接，并且服务端确实返回了可用的加密会话。
func TestOpenVerifyFullWithPrivateCA(t *testing.T) {
	pg := testenv.StartPostgresTLS(t, "expense")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, pg.AppDSN("expense"), 2)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, pool.Ping(ctx))

	var ssl string
	err = pool.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SHOW ssl").Scan(&ssl)
	})
	require.NoError(t, err)
	assert.Equal(t, "on", ssl, "服务端必须开启 TLS 才谈得上 verify-full")
}

// TestOpenVerifyFullRejectsWrongCA 保证证书链真的被校验：
// 换一张合法但不同的 CA，握手必须失败，而不是悄悄降级。
func TestOpenVerifyFullRejectsWrongCA(t *testing.T) {
	pg := testenv.StartPostgresTLS(t, "expense")
	wrongCA := writeOtherCA(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, pg.AppDSNWithCA("expense", wrongCA), 2)
	if pool != nil {
		pool.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
}

// writeOtherCA 生成一张与测试服务端无关的自签 CA，只用于负例。
func writeOtherCA(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(99),
		Subject:               pkix.Name{CommonName: "expensio-wrong-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "wrong-ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	return path
}
