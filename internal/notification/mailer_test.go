package notification

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSMTPServer 是一个只实现测试所需命令的最小 SMTP 服务：
// EHLO / STARTTLS / AUTH PLAIN / MAIL / RCPT / DATA / QUIT。
// 它让 mailer 的 TLS 与认证分支跑在真实 socket 上，而不是被打桩绕过。
type fakeSMTPServer struct {
	addr    string
	done    chan error
	gotAuth bool
	message string
}

func startFakeSMTP(t *testing.T, advertiseSTARTTLS bool, wantAuthPlain string) (*fakeSMTPServer, *x509.CertPool) {
	t.Helper()

	cert, pool := testSMTPCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	srv := &fakeSMTPServer{addr: listener.Addr().String(), done: make(chan error, 1)}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			srv.done <- err
			return
		}
		srv.done <- srv.serve(conn, cert, advertiseSTARTTLS, wantAuthPlain)
	}()
	return srv, pool
}

func (s *fakeSMTPServer) serve(conn net.Conn, cert tls.Certificate, advertiseSTARTTLS bool, wantAuthPlain string) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	write := func(line string) error {
		_, err := conn.Write([]byte(line + "\r\n"))
		return err
	}
	var tlsConn *tls.Conn

	if err := write("220 fake ESMTP"); err != nil {
		return err
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// 客户端在错误路径上直接关闭连接，是预期行为。
			return nil
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"):
			caps := []string{"250-fake"}
			if advertiseSTARTTLS && tlsConn == nil {
				caps = append(caps, "250-STARTTLS")
			}
			// PLAIN 只在 TLS 之后公布：net/smtp 的 PlainAuth 也拒绝明文认证。
			if wantAuthPlain != "" && tlsConn != nil {
				caps = append(caps, "250-AUTH PLAIN")
			}
			caps = append(caps, "250 OK")
			if err := write(strings.Join(caps, "\r\n")); err != nil {
				return err
			}
		case strings.EqualFold(line, "STARTTLS"):
			if !advertiseSTARTTLS || tlsConn != nil {
				return fmt.Errorf("测试服务器收到意外的 STARTTLS")
			}
			if err := write("220 Ready to start TLS"); err != nil {
				return err
			}
			tc := tls.Server(conn, &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			})
			if err := tc.Handshake(); err != nil {
				return fmt.Errorf("测试服务器 TLS 握手失败: %w", err)
			}
			tlsConn = tc
			conn = tc
			reader = bufio.NewReader(conn)
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] != wantAuthPlain {
				return fmt.Errorf("测试服务器收到错误的认证: %q", line)
			}
			s.gotAuth = true
			if err := write("235 2.7.0 Authentication successful"); err != nil {
				return err
			}
		case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
			if err := write("250 OK"); err != nil {
				return err
			}
		case upper == "DATA":
			if err := write("354 End data with <CR><LF>.<CR><LF>"); err != nil {
				return err
			}
			var body strings.Builder
			for {
				part, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				if part == ".\r\n" {
					break
				}
				body.WriteString(part)
			}
			s.message = body.String()
			if err := write("250 OK queued"); err != nil {
				return err
			}
		case upper == "QUIT":
			_ = write("221 Bye")
			return nil
		default:
			if err := write("250 OK"); err != nil {
				return err
			}
		}
	}
}

func (s *fakeSMTPServer) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-s.done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("测试 SMTP 服务未在 5 秒内结束")
	}
}

func testSMTPCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "expensio-test-smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))
	return cert, pool
}

func plainAuthResponse(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + password))
}

func TestSMTPMailerSendsOverSTARTTLSWithAuth(t *testing.T) {
	const (
		user     = "smtp-user"
		password = "smtp-pass"
	)
	srv, pool := startFakeSMTP(t, true, plainAuthResponse(user, password))

	mailer, err := NewSMTPMailer(MailerOptions{
		Addr:       srv.addr,
		From:       "no-reply@example.com",
		Username:   user,
		Password:   password,
		RequireTLS: true,
		Timeout:    5 * time.Second,
		TLSConfig:  &tls.Config{RootCAs: pool},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, mailer.Send(ctx, "invitee@example.com", "邀请测试", "请打开链接\n第二行"))
	srv.wait(t)

	assert.True(t, srv.gotAuth, "应通过 STARTTLS 后的 AUTH PLAIN 认证")
	assert.Contains(t, srv.message, "To: invitee@example.com")
	assert.Contains(t, srv.message, "请打开链接")
}

func TestSMTPMailerRequiresStartTLSWhenConfigured(t *testing.T) {
	srv, _ := startFakeSMTP(t, false, "")

	mailer, err := NewSMTPMailer(MailerOptions{
		Addr:       srv.addr,
		From:       "no-reply@example.com",
		RequireTLS: true,
		Timeout:    5 * time.Second,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = mailer.Send(ctx, "invitee@example.com", "邀请测试", "body")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持 STARTTLS")
	srv.wait(t)
}

func TestSMTPMailerRejectsUntrustedCertificate(t *testing.T) {
	srv, _ := startFakeSMTP(t, true, "")

	mailer, err := NewSMTPMailer(MailerOptions{
		Addr:       srv.addr,
		From:       "no-reply@example.com",
		RequireTLS: true,
		Timeout:    5 * time.Second,
		// 空 RootCAs 会退回系统根证书，自签证书必须校验失败。
		TLSConfig: &tls.Config{},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = mailer.Send(ctx, "invitee@example.com", "邀请测试", "body")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STARTTLS 失败")
	assert.Contains(t, err.Error(), "certificate")

	// 服务端的握手会因客户端拒绝证书而失败；这里只确认 goroutine 退出。
	select {
	case <-srv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("测试 SMTP 服务未退出")
	}
}
