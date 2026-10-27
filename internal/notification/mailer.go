package notification

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Mailer 是投递一封纯文本邮件的抽象。worker 只依赖这个接口，
// 测试里可以用内存实现替换 SMTP。
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// MailerOptions 是 SMTP 连接参数。口令只从环境变量 / K8s Secret 注入。
type MailerOptions struct {
	Addr     string
	From     string
	Username string
	Password string
	// RequireTLS 为 true 时，服务器不支持 STARTTLS 就拒绝发送。
	// 本地 Mailpit 不提供 TLS，生产环境应设为 true。
	RequireTLS bool
	Timeout    time.Duration
	// TLSConfig 可选：用于自定义 STARTTLS 握手（企业内网自签 CA、测试）。
	// nil 时使用系统根证书，ServerName 取 SMTP 主机名，最低 TLS 1.2。
	TLSConfig *tls.Config
}

// SMTPMailer 用标准库 net/smtp 发送纯文本邮件。
// 不引入第三方客户端：这里只需要 Mail/Rcpt/Data 三个动作。
type SMTPMailer struct {
	addr       string
	host       string
	from       string
	username   string
	password   string
	requireTLS bool
	timeout    time.Duration
	tlsConfig  *tls.Config
}

func NewSMTPMailer(opts MailerOptions) (*SMTPMailer, error) {
	host, _, err := net.SplitHostPort(opts.Addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("notification: SMTP 地址非法: %q", opts.Addr)
	}
	if strings.ContainsAny(opts.From, "\r\n") || !strings.Contains(opts.From, "@") {
		return nil, fmt.Errorf("notification: 发件人地址非法: %q", opts.From)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	return &SMTPMailer{
		addr:       opts.Addr,
		host:       host,
		from:       opts.From,
		username:   opts.Username,
		password:   opts.Password,
		requireTLS: opts.RequireTLS,
		timeout:    opts.Timeout,
		tlsConfig:  opts.TLSConfig,
	}, nil
}

func (m *SMTPMailer) Send(ctx context.Context, to, subject, body string) error {
	if strings.ContainsAny(to, "\r\n") || !strings.Contains(to, "@") {
		return fmt.Errorf("notification: 收件人地址非法")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("notification: 邮件主题包含换行")
	}

	dialer := net.Dialer{Timeout: m.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return fmt.Errorf("notification: 连接 SMTP 失败: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(m.timeout))
	}
	// conn.Close 在 Quit 失败时兜底；正常路径下 Quit 已经关闭连接。
	defer conn.Close()

	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return fmt.Errorf("notification: SMTP 握手失败: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(m.startTLSConfig()); err != nil {
			return fmt.Errorf("notification: STARTTLS 失败: %w", err)
		}
	} else if m.requireTLS {
		return fmt.Errorf("notification: SMTP 服务器不支持 STARTTLS")
	}

	if m.username != "" {
		auth := smtp.PlainAuth("", m.username, m.password, m.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notification: SMTP 认证失败: %w", err)
		}
	}

	if err := client.Mail(m.from); err != nil {
		return fmt.Errorf("notification: SMTP MAIL 失败: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("notification: SMTP RCPT 失败: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("notification: SMTP DATA 失败: %w", err)
	}
	msg := buildMessage(m.from, to, subject, body)
	if _, err := w.Write([]byte(msg)); err != nil {
		_ = w.Close()
		return fmt.Errorf("notification: 写入邮件正文失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("notification: 结束邮件正文失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("notification: SMTP QUIT 失败: %w", err)
	}
	return nil
}

// startTLSConfig 在默认安全参数上叠加调用方配置；调用方没写的字段
// 仍按安全默认填充，避免传入 &tls.Config{} 时意外接受低版本或空 ServerName。
func (m *SMTPMailer) startTLSConfig() *tls.Config {
	cfg := &tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12}
	if m.tlsConfig == nil {
		return cfg
	}
	cfg = m.tlsConfig.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = m.host
	}
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return cfg
}

// buildMessage 生成 RFC 5322 纯文本邮件。主题用 RFC 2047 编码，
// 否则中文标题在部分邮件客户端会变成乱码。
func buildMessage(from, to, subject, body string) string {
	encodedSubject := mime.QEncoding.Encode("UTF-8", subject)
	normalized := strings.ReplaceAll(body, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\n", "\r\n")

	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodedSubject + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalized)
	if !strings.HasSuffix(normalized, "\r\n") {
		b.WriteString("\r\n")
	}
	return b.String()
}
