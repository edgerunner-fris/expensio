// Package config 负责从环境变量加载并校验服务配置。
//
// 四条约束：
//   - 每个配置项都先注册默认值再 Unmarshal。viper 的 Unmarshal 只认识"已知键"，
//     没有默认值的键即使设了环境变量也会被静默忽略——这是 viper 最常见的事故。
//   - 环境变量统一加 EXPENSIO_ 前缀，键里的 "." 换成 "_"。
//   - 校验一次性汇总所有问题，而不是遇到第一个就返回，避免"改一个报一个"。
//   - 密码类字段实现 Stringer 时显式打码，fmt %v 不会带出明文。
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Options 描述"这个服务需要哪些依赖"。
// 是否校验某个配置段由它决定：gateway 不需要数据库，就不该因为 DB_NAME 为空而启动失败。
type Options struct {
	Service     string
	DefaultAddr string
	NeedsDB     bool
	NeedsRedis  bool
	NeedsKafka  bool
	NeedsJWT    bool
	NeedsMinIO  bool
	// NeedsJWTSigner 只在 identity 为 true：它是唯一持有 Ed25519 私钥、
	// 可以签发 access token 的服务。其他服务只拿公钥验签。
	NeedsJWTSigner bool
	// NeedsBillingWebhook 只在 billing 为 true：回调密钥与渠道名是运行必需项，
	// 缺失时应在启动期失败，而不是等第一个回调进来才发现没配置。
	NeedsBillingWebhook bool
	// NeedsInternalAPI 只在 identity 为 true：服务间联系人接口必须校验内部令牌。
	NeedsInternalAPI bool
	// NeedsNotification 只在 notification 为 true：内部令牌、identity 地址与 SMTP
	// 都是运行必需项，缺失时启动期就失败。
	NeedsNotification bool
	// NeedsNotificationClient 只在 identity 为 true：邀请邮件要投递到 notification
	// 的内部接口，因此必须知道它的地址与对外站点地址（用于拼接邀请链接）。
	NeedsNotificationClient bool
	// NeedsUpstreams 只在网关为 true：其他服务是"被转发方"，没有下游地址可校验。
	NeedsUpstreams bool
}

// Config 是所有服务共享的配置结构，未用到的段留空即可。
type Config struct {
	Service   string
	Env       string    `mapstructure:"env"`
	LogLevel  string    `mapstructure:"log_level"`
	HTTP      HTTP      `mapstructure:"http"`
	DB        DB        `mapstructure:"db"`
	Redis     Redis     `mapstructure:"redis"`
	Kafka     Kafka     `mapstructure:"kafka"`
	JWT       JWT       `mapstructure:"jwt"`
	OTel      OTel      `mapstructure:"otel"`
	MinIO     MinIO     `mapstructure:"minio"`
	Billing   Billing   `mapstructure:"billing"`
	Notify    Notify    `mapstructure:"notification"`
	Invite    Invite    `mapstructure:"invite"`
	Upstreams Upstreams `mapstructure:"upstreams"`
	RateLimit RateLimit `mapstructure:"rate_limit"`
}

// Invite 是成员邀请的配置。BaseURL 只用于拼对外的接受链接，
// 必须与用户实际访问的站点一致；TTL 决定链接在邮件里写明的有效期。
type Invite struct {
	BaseURL string        `mapstructure:"base_url"`
	TTL     time.Duration `mapstructure:"ttl"`
}

// Upstreams 是网关转发目标。值是完整 base URL（scheme + host + port），
// 不允许带路径前缀：带前缀之后，"前缀 + 原始路径"的拼接规则就会变得难以推理。
type Upstreams struct {
	Identity     string `mapstructure:"identity"`
	Expense      string `mapstructure:"expense"`
	Billing      string `mapstructure:"billing"`
	Report       string `mapstructure:"report"`
	Notification string `mapstructure:"notification"`
}

// RateLimit 是网关的令牌桶参数：每分钟补充多少、桶有多大。
// burst 决定瞬时容忍度，rate 决定长期速率，两者分开才能既抗突发又限总量。
type RateLimit struct {
	AuthPerMinute int `mapstructure:"auth_per_minute"`
	AuthBurst     int `mapstructure:"auth_burst"`
	APIPerMinute  int `mapstructure:"api_per_minute"`
	APIBurst      int `mapstructure:"api_burst"`
}

type HTTP struct {
	Addr            string        `mapstructure:"addr"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// DrainDelay 是 readiness 置 false 之后、真正关闭监听之前的等待时间。
	// 本地为 0；K8s 里要留够 kube-proxy 把本实例从 Endpoints 摘掉的时间。
	DrainDelay time.Duration `mapstructure:"drain_delay"`
	// TrustedProxies 是允许携带 X-Forwarded-For 的上游地址，逗号分隔的 IP / CIDR。
	// 为空表示不信任任何代理头。网关经 web 反代或 ingress 接入时，
	// 必须把代理网段配进来，否则 ClientIP 永远等于代理 IP，
	// 按 IP 限流就会退化成"全站共用一个桶"。
	TrustedProxies string `mapstructure:"trusted_proxies"`
}

// TrustedProxyList 解析并校验 HTTP.TrustedProxies。
// 拒绝 0 位前缀：信任 0.0.0.0/0 等于让任何人都能伪造来源 IP。
func (h HTTP) TrustedProxyList() ([]string, error) {
	raw := strings.TrimSpace(h.TrustedProxies)
	if raw == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			return nil, errors.New("http.trusted_proxies 含空项（应为逗号分隔的 IP 或 CIDR）")
		}
		if net.ParseIP(p) == nil {
			_, ipnet, err := net.ParseCIDR(p)
			if err != nil {
				return nil, fmt.Errorf("http.trusted_proxies 中的 %q 不是合法 IP 或 CIDR", p)
			}
			if ones, _ := ipnet.Mask.Size(); ones == 0 {
				return nil, fmt.Errorf("http.trusted_proxies 不允许 %q：0 位前缀会信任所有来源", p)
			}
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

type DB struct {
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	User        string `mapstructure:"user"`
	Password    string `mapstructure:"password"`
	Name        string `mapstructure:"name"`
	SSLMode     string `mapstructure:"sslmode"`
	MaxConns    int32  `mapstructure:"max_conns"`
	DSNOverride string `mapstructure:"dsn"`
}

type Redis struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type Kafka struct {
	Brokers []string `mapstructure:"brokers"`
}

type JWT struct {
	Issuer     string        `mapstructure:"issuer"`
	AccessTTL  time.Duration `mapstructure:"access_ttl"`
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
	// PrivateKey 是 base64 的 Ed25519 私钥（32 字节种子或 64 字节私钥），
	// 只有 identity 配置。PublicKeys 是逗号分隔的 base64 公钥，
	// 所有验签方配置；保留多个是为了轮换期同时接受新旧 kid。
	// 只支持 EdDSA：不接受 HS256 等共享密钥算法。
	PrivateKey string `mapstructure:"private_key"`
	PublicKeys string `mapstructure:"public_keys"`
}

// OTel 控制 OTLP 导出。Enabled=false 时进程使用 noop provider，
// 本地裸跑不要求先起 Collector；容器编排里显式打开。
type OTel struct {
	Enabled  bool   `mapstructure:"enabled"`
	Endpoint string `mapstructure:"endpoint"`
	Insecure bool   `mapstructure:"insecure"`
}

type MinIO struct {
	Endpoint string `mapstructure:"endpoint"`
	// PublicEndpoint 是浏览器访问对象存储的地址，只影响 presigned URL 里的 Host。
	// 留空时与 Endpoint 相同，适用于本机直跑。
	PublicEndpoint string `mapstructure:"public_endpoint"`
	AccessKey      string `mapstructure:"access_key"`
	SecretKey      string `mapstructure:"secret_key"`
	Bucket         string `mapstructure:"bucket"`
	UseSSL         bool   `mapstructure:"use_ssl"`
}

// Billing 是支付回调的验签参数。密钥只从环境变量 / K8s Secret 读取，
// 没有默认值；渠道名只用于拒绝其他渠道的回调。
type Billing struct {
	WebhookProvider string `mapstructure:"webhook_provider"`
	WebhookSecret   string `mapstructure:"webhook_secret"`
}

// Notify 是 notification 与 identity 之间共享的内部通知配置。
// InternalToken 只从环境变量 / K8s Secret 读取；SMTP 口令同样不落库、不入仓。
type Notify struct {
	InternalToken string `mapstructure:"internal_token"`
	IdentityURL   string `mapstructure:"identity_url"`
	// ServiceURL 只有 identity 使用：它把邀请邮件投递到 notification 的内部接口。
	ServiceURL   string `mapstructure:"service_url"`
	EmailEnabled bool   `mapstructure:"email_enabled"`
	SMTPAddr     string `mapstructure:"smtp_addr"`
	SMTPFrom     string `mapstructure:"smtp_from"`
	SMTPUsername string `mapstructure:"smtp_username"`
	SMTPPassword string `mapstructure:"smtp_password"`
	// SMTPUseTLS 为 true 时强制 STARTTLS；本地 Mailpit 不提供 TLS，默认关闭。
	SMTPUseTLS bool `mapstructure:"smtp_use_tls"`
}

// Load 读取环境变量、应用默认值、反序列化并校验。
func Load(opts Options) (*Config, error) {
	if opts.Service == "" {
		return nil, errors.New("config: 必须指定服务名")
	}

	v := viper.New()
	v.SetEnvPrefix("EXPENSIO")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	for key, val := range defaults(opts) {
		v.SetDefault(key, val)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config: 解析配置失败: %w", err)
	}

	cfg.Service = opts.Service
	if cfg.HTTP.Addr == "" {
		cfg.HTTP.Addr = opts.DefaultAddr
	}
	if err := cfg.validate(opts); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// defaults 必须覆盖 all 配置键，否则对应环境变量会被 viper 忽略。
func defaults(opts Options) map[string]any {
	return map[string]any{
		"env":       "local",
		"log_level": "info",

		"http.addr":             opts.DefaultAddr,
		"http.read_timeout":     "5s",
		"http.write_timeout":    "10s",
		"http.idle_timeout":     "60s",
		"http.shutdown_timeout": "20s",
		"http.drain_delay":      "0s",
		"http.trusted_proxies":  "",

		"db.host":      "localhost",
		"db.port":      5432,
		"db.user":      "expensio",
		"db.password":  "",
		"db.name":      "",
		"db.sslmode":   "disable",
		"db.max_conns": 10,
		"db.dsn":       "",

		"redis.addr":     "localhost:6379",
		"redis.password": "",
		"redis.db":       0,

		"kafka.brokers": []string{"localhost:29092"},

		"jwt.issuer":      "expensio",
		"jwt.access_ttl":  "15m",
		"jwt.refresh_ttl": "168h",
		"jwt.private_key": "",
		"jwt.public_keys": "",

		"otel.enabled":  false,
		"otel.endpoint": "localhost:4317",
		"otel.insecure": true,

		"minio.endpoint":   "localhost:9000",
		"minio.access_key": "",
		"minio.secret_key": "",
		"minio.bucket":     "expensio-invoices",
		"minio.use_ssl":    false,
		// 空表示沿用 endpoint；容器内地址与浏览器地址不同时再覆盖。
		"minio.public_endpoint": "",

		"billing.webhook_provider": "mockpay",
		"billing.webhook_secret":   "",

		"notification.internal_token": "",
		"notification.identity_url":   "http://127.0.0.1:8081",
		"notification.service_url":    "http://127.0.0.1:8085",
		"notification.email_enabled":  true,
		"notification.smtp_addr":      "localhost:1025",
		"notification.smtp_from":      "no-reply@expensio.local",
		"notification.smtp_username":  "",
		"notification.smtp_password":  "",
		"notification.smtp_use_tls":   false,

		"invite.base_url": "http://127.0.0.1:5173",
		"invite.ttl":      "168h",

		// 网关下游：本地默认指向 make run-<service> 起的端口。
		"upstreams.identity":     "http://127.0.0.1:8081",
		"upstreams.expense":      "http://127.0.0.1:8082",
		"upstreams.billing":      "http://127.0.0.1:8083",
		"upstreams.report":       "http://127.0.0.1:8084",
		"upstreams.notification": "http://127.0.0.1:8085",

		// 认证类接口按 IP 限流（防撞库），业务接口按租户限流（防单租户拖垮全局）。
		"rate_limit.auth_per_minute": 20,
		"rate_limit.auth_burst":      10,
		"rate_limit.api_per_minute":  1200,
		"rate_limit.api_burst":       200,
	}
}

func (c *Config) validate(opts Options) error {
	var errs []error

	switch c.Env {
	case "local", "dev", "prod":
	default:
		errs = append(errs, fmt.Errorf("env 必须是 local/dev/prod 之一，当前为 %q", c.Env))
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level 必须是 debug/info/warn/error 之一，当前为 %q", c.LogLevel))
	}

	if _, port, err := net.SplitHostPort(c.HTTP.Addr); err != nil {
		errs = append(errs, fmt.Errorf("http.addr 非法（应形如 :8080），当前为 %q", c.HTTP.Addr))
	} else if p, convErr := strconv.Atoi(port); convErr != nil || p < 1 || p > 65535 {
		errs = append(errs, fmt.Errorf("http.addr 端口非法，当前为 %q", c.HTTP.Addr))
	}

	if c.HTTP.ShutdownTimeout <= c.HTTP.ReadTimeout {
		errs = append(errs, errors.New("http.shutdown_timeout 必须大于 http.read_timeout"))
	}
	// net/http 的 Shutdown 会把"TCP 已连上但还没发来请求"的连接当作活跃，
	// 直到它建立满 5 秒（golang/go#22682）。关闭预算低于 5s 时，
	// 一个浏览器预连接就足以让正常关闭被误判成超时。
	if c.HTTP.ShutdownTimeout <= 5*time.Second {
		errs = append(errs, errors.New("http.shutdown_timeout 必须大于 5s（net/http 对未发请求连接的宽限期）"))
	}
	if c.HTTP.DrainDelay < 0 {
		errs = append(errs, errors.New("http.drain_delay 不能为负"))
	}
	if _, err := c.HTTP.TrustedProxyList(); err != nil {
		errs = append(errs, err)
	}

	if opts.NeedsDB {
		if c.DB.Host == "" {
			errs = append(errs, errors.New("db.host 不能为空（EXPENSIO_DB_HOST）"))
		}
		if c.DB.Name == "" {
			errs = append(errs, errors.New("db.name 不能为空（EXPENSIO_DB_NAME）"))
		}
		if c.DB.User == "" {
			errs = append(errs, errors.New("db.user 不能为空（EXPENSIO_DB_USER）"))
		}
		if c.DB.Port < 1 || c.DB.Port > 65535 {
			errs = append(errs, fmt.Errorf("db.port 非法，当前为 %d", c.DB.Port))
		}
		if c.DB.MaxConns < 1 {
			errs = append(errs, fmt.Errorf("db.max_conns 必须大于 0，当前为 %d", c.DB.MaxConns))
		}
	}

	if opts.NeedsRedis && c.Redis.Addr == "" {
		errs = append(errs, errors.New("redis.addr 不能为空（EXPENSIO_REDIS_ADDR）"))
	}

	if opts.NeedsKafka && len(c.Kafka.Brokers) == 0 {
		errs = append(errs, errors.New("kafka.brokers 不能为空（EXPENSIO_KAFKA_BROKERS）"))
	}

	if opts.NeedsJWT {
		if c.JWT.Issuer == "" {
			errs = append(errs, errors.New("jwt.issuer 不能为空（EXPENSIO_JWT_ISSUER）"))
		}
		if c.JWT.AccessTTL <= 0 || c.JWT.RefreshTTL <= 0 {
			errs = append(errs, errors.New("jwt.access_ttl 与 jwt.refresh_ttl 必须为正"))
		}
		if c.JWT.AccessTTL >= c.JWT.RefreshTTL {
			errs = append(errs, errors.New("jwt.access_ttl 必须小于 jwt.refresh_ttl"))
		}
		if opts.NeedsJWTSigner {
			if c.JWT.PrivateKey == "" {
				errs = append(errs, errors.New("jwt.private_key 不能为空（EXPENSIO_JWT_PRIVATE_KEY）：签发方必须配置 Ed25519 私钥"))
			}
		} else if strings.TrimSpace(c.JWT.PublicKeys) == "" {
			// 只验签的服务没有私钥，必须至少配置一个公钥；否则它无法校验任何令牌。
			errs = append(errs, errors.New("jwt.public_keys 不能为空（EXPENSIO_JWT_PUBLIC_KEYS）：验签方必须配置 Ed25519 公钥"))
		}
	}

	if opts.NeedsMinIO {
		if c.MinIO.Endpoint == "" {
			errs = append(errs, errors.New("minio.endpoint 不能为空（EXPENSIO_MINIO_ENDPOINT）"))
		}
		if c.MinIO.Bucket == "" {
			errs = append(errs, errors.New("minio.bucket 不能为空（EXPENSIO_MINIO_BUCKET）"))
		}
	}

	if opts.NeedsBillingWebhook {
		// 与 billing.EnableWebhooks 的下限一致：32 字节以下的 HMAC 密钥
		// 不足以抵抗离线暴力破解，直接拒绝启动。
		if len(c.Billing.WebhookSecret) < 32 {
			errs = append(errs, errors.New("billing.webhook_secret 至少需要 32 字节（EXPENSIO_BILLING_WEBHOOK_SECRET）"))
		}
		if c.Billing.WebhookProvider == "" {
			errs = append(errs, errors.New("billing.webhook_provider 不能为空（EXPENSIO_BILLING_WEBHOOK_PROVIDER）"))
		}
	}

	if opts.NeedsInternalAPI || opts.NeedsNotification {
		if len(c.Notify.InternalToken) < 32 {
			errs = append(errs, errors.New("notification.internal_token 至少需要 32 字节（EXPENSIO_NOTIFICATION_INTERNAL_TOKEN）"))
		}
	}

	if opts.NeedsNotificationClient {
		parsed, err := url.Parse(c.Notify.ServiceURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") {
			errs = append(errs, fmt.Errorf("notification.service_url 必须是完整的 http(s) 地址且不带路径，当前为 %q", c.Notify.ServiceURL))
		}
		invite, err := url.Parse(c.Invite.BaseURL)
		if err != nil || invite.Scheme == "" || invite.Host == "" {
			errs = append(errs, fmt.Errorf("invite.base_url 必须是完整的 http(s) 地址，当前为 %q", c.Invite.BaseURL))
		}
		if c.Invite.TTL < time.Hour || c.Invite.TTL > 30*24*time.Hour {
			errs = append(errs, fmt.Errorf("invite.ttl 必须在 1h 到 720h 之间，当前为 %s", c.Invite.TTL))
		}
	}

	if opts.NeedsNotification {
		parsed, err := url.Parse(c.Notify.IdentityURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("notification.identity_url 必须是完整的 http(s) 地址，当前为 %q", c.Notify.IdentityURL))
		}
		if c.Notify.EmailEnabled {
			if _, port, err := net.SplitHostPort(c.Notify.SMTPAddr); err != nil {
				errs = append(errs, fmt.Errorf("notification.smtp_addr 非法（应形如 host:port），当前为 %q", c.Notify.SMTPAddr))
			} else if p, convErr := strconv.Atoi(port); convErr != nil || p < 1 || p > 65535 {
				errs = append(errs, fmt.Errorf("notification.smtp_addr 端口非法，当前为 %q", c.Notify.SMTPAddr))
			}
			if !strings.Contains(c.Notify.SMTPFrom, "@") {
				errs = append(errs, fmt.Errorf("notification.smtp_from 必须是邮箱地址，当前为 %q", c.Notify.SMTPFrom))
			}
		}
	}

	if c.OTel.Enabled {
		// Endpoint 是 host:port，不带 scheme；带 scheme 的写法会被 gRPC 当作
		// 拨号目标解析失败，只有等到第一次导出才报错，排查成本很高。
		if _, port, err := net.SplitHostPort(c.OTel.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("otel.endpoint 非法（应形如 host:4317），当前为 %q", c.OTel.Endpoint))
		} else if p, convErr := strconv.Atoi(port); convErr != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("otel.endpoint 端口非法，当前为 %q", c.OTel.Endpoint))
		}
	}

	if opts.NeedsUpstreams {
		for _, u := range []struct{ name, raw string }{
			{"upstreams.identity", c.Upstreams.Identity},
			{"upstreams.expense", c.Upstreams.Expense},
			{"upstreams.billing", c.Upstreams.Billing},
			{"upstreams.report", c.Upstreams.Report},
			{"upstreams.notification", c.Upstreams.Notification},
		} {
			parsed, err := url.Parse(u.raw)
			switch {
			case err != nil || parsed.Scheme == "" || parsed.Host == "":
				errs = append(errs, fmt.Errorf("%s 必须是完整的 http(s) 地址，当前为 %q", u.name, u.raw))
			case parsed.Path != "" && parsed.Path != "/":
				errs = append(errs, fmt.Errorf("%s 不能带路径前缀，当前为 %q", u.name, u.raw))
			}
		}
		if c.RateLimit.AuthPerMinute <= 0 || c.RateLimit.AuthBurst <= 0 ||
			c.RateLimit.APIPerMinute <= 0 || c.RateLimit.APIBurst <= 0 {
			errs = append(errs, errors.New("rate_limit 的速率与桶容量都必须大于 0"))
		}
	}

	return errors.Join(errs...)
}

// DSN 构造 PostgreSQL 连接串。
// 用 url.URL 而不是字符串拼接：口令里出现 @ / : % 时不会破坏连接串。
func (d DB) DSN() string {
	if d.DSNOverride != "" {
		return d.DSNOverride
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(d.User, d.Password),
		Host:   net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:   "/" + d.Name,
	}
	q := u.Query()
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// String 打码，避免配置被日志或 panic 打印时泄露口令。
func (d DB) String() string {
	return fmt.Sprintf("DB{host:%s port:%d name:%s user:%s password:***}",
		d.Host, d.Port, d.Name, d.User)
}

func (r Redis) String() string {
	return fmt.Sprintf("Redis{addr:%s db:%d}", r.Addr, r.DB)
}

func (j JWT) String() string {
	return fmt.Sprintf("JWT{issuer:%s access_ttl:%s refresh_ttl:%s ed25519:%t}",
		j.Issuer, j.AccessTTL, j.RefreshTTL,
		j.PrivateKey != "" || j.PublicKeys != "")
}

func (m MinIO) String() string {
	return fmt.Sprintf("MinIO{endpoint:%s bucket:%s access_key:*** secret_key:***}",
		m.Endpoint, m.Bucket)
}

func (b Billing) String() string {
	return fmt.Sprintf("Billing{webhook_provider:%s webhook_secret:***}",
		b.WebhookProvider)
}

func (n Notify) String() string {
	return fmt.Sprintf("Notify{identity_url:%s email_enabled:%t smtp_addr:%s smtp_from:%s internal_token:*** smtp_password:***}",
		n.IdentityURL, n.EmailEnabled, n.SMTPAddr, n.SMTPFrom)
}

func (c Config) String() string {
	return fmt.Sprintf("Config{service:%s env:%s log_level:%s http:%+v db:%s redis:%s jwt:%s billing:%s notify:%s}",
		c.Service, c.Env, c.LogLevel, c.HTTP, c.DB, c.Redis, c.JWT, c.Billing, c.Notify)
}
