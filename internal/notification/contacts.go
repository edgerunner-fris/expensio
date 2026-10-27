package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
)

// ContactsSource 是"按租户取收件人"的抽象，便于消费端测试注入。
type ContactsSource interface {
	TenantContacts(ctx context.Context, tenantID uuid.UUID) ([]Contact, error)
}

// ContactsClient 通过 identity 的内部接口取收件人。
//
// 租户只出现在签名令牌的载荷里：URL、query、body 都不携带 tenant_id，
// 即使请求被代理日志完整记录，也无法据此冒充别的租户。
type ContactsClient struct {
	baseURL string
	secret  string
	client  *http.Client
	ttl     time.Duration
}

const contactsMaxBody = 1 << 20 // 1 MiB 足以容纳一个租户的联系人列表

// NewContactsClient 校验配置并构造客户端。失败应在启动期暴露，
// 而不是等第一条事件到达才发现 identity_url 写错。
func NewContactsClient(baseURL, secret string, timeout time.Duration) (*ContactsClient, error) {
	if len(secret) < serviceauth.MinSecretLen {
		return nil, fmt.Errorf("notification: 内部令牌至少需要 %d 字节", serviceauth.MinSecretLen)
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("notification: identity 地址非法: %q", baseURL)
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &ContactsClient{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		secret:  secret,
		client: &http.Client{
			Timeout:   timeout,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		ttl: 2 * time.Minute,
	}, nil
}

type contactsResponse struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Data    []Contact `json:"data"`
}

// TenantContacts 拉取一个租户的全部有效成员。任何非 200 或非 ok 响应都视为
// 瞬时失败：消费端会重试，事件不会丢；但错误信息只保留状态码，不带响应体
// （响应体可能包含邮箱）。
func (c *ContactsClient) TenantContacts(ctx context.Context, tenantID uuid.UUID) ([]Contact, error) {
	token, err := serviceauth.Sign(c.secret, tenantID, c.ttl, time.Now())
	if err != nil {
		return nil, fmt.Errorf("notification: 签发内部令牌失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/internal/tenant-contacts", nil)
	if err != nil {
		return nil, fmt.Errorf("notification: 构造联系人请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notification: 请求联系人失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 丢弃响应体，避免把内部错误细节或邮箱写进日志。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, contactsMaxBody))
		return nil, fmt.Errorf("notification: 联系人接口返回 %d", resp.StatusCode)
	}

	var body contactsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, contactsMaxBody))
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("notification: 解析联系人响应失败: %w", err)
	}
	if body.Code != "ok" {
		return nil, fmt.Errorf("notification: 联系人接口业务码异常: %q", body.Code)
	}
	if body.Data == nil {
		body.Data = make([]Contact, 0)
	}
	return body.Data, nil
}
