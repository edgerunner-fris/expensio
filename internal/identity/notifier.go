package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/serviceauth"
)

// InvitationEmail 是投递一封邀请邮件所需的全部参数。
// 明文令牌只出现在 Link 里，不会写进数据库或日志。
type InvitationEmail struct {
	InvitationID uuid.UUID
	Recipient    string
	TenantName   string
	Link         string
	ExpiresAt    time.Time
}

// EmailNotifier 把邮件交给 notification 服务。identity 不直接连 SMTP：
// 发信是 notification 的职责，它已经有发件箱与重试 worker。
type EmailNotifier interface {
	EnqueueInvitation(ctx context.Context, tenantID uuid.UUID, in InvitationEmail) error
}

// HTTPNotifier 通过服务间签名令牌调用 notification 的内部接口。
type HTTPNotifier struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPNotifier 校验配置并构造客户端。baseURL 不允许带路径，
// 否则拼接 /api/v1/internal/emails 时会产生难以推理的地址。
func NewHTTPNotifier(baseURL, internalToken string, timeout time.Duration) (*HTTPNotifier, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("identity: notification 服务地址不能为空")
	}
	if len(internalToken) < serviceauth.MinSecretLen {
		return nil, fmt.Errorf("identity: 内部令牌至少需要 %d 字节", serviceauth.MinSecretLen)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPNotifier{
		baseURL: base,
		token:   internalToken,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

type internalEmailPayload struct {
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	DedupeKey string `json:"dedupe_key"`
}

// EnqueueInvitation 把邮件写入 notification 的发件箱。
// 令牌只要覆盖一次重试窗口即可：投递本身在 outbox 里完成，
// 不需要长期有效的服务间凭据。
func (n *HTTPNotifier) EnqueueInvitation(ctx context.Context, tenantID uuid.UUID, in InvitationEmail) error {
	signed, err := serviceauth.Sign(n.token, tenantID, time.Minute, time.Now())
	if err != nil {
		return fmt.Errorf("identity: 签发服务间令牌失败: %w", err)
	}

	payload := internalEmailPayload{
		Recipient: in.Recipient,
		Subject:   fmt.Sprintf("加入 %s 的 Expensio 团队", in.TenantName),
		Body: fmt.Sprintf(
			"你好，\n\n%s 邀请你加入 Expensio 费用管理平台。\n\n"+
				"点击下面的链接设置密码并加入团队：\n%s\n\n"+
				"邀请有效期至 %s（UTC）。如果不是你本人，请忽略这封邮件。\n",
			in.TenantName, in.Link, in.ExpiresAt.UTC().Format("2006-01-02 15:04"),
		),
		DedupeKey: "invitation:" + in.InvitationID.String(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("identity: 序列化邀请邮件失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.baseURL+"/api/v1/internal/emails", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("identity: 构造邀请邮件请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signed)

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("identity: 调用 notification 失败: %w", err)
	}
	defer resp.Body.Close()
	// 读取并丢弃响应体，连接才能复用；响应内容不进入错误信息。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("identity: notification 返回状态 %d", resp.StatusCode)
	}
	return nil
}
