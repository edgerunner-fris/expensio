// Package serviceauth 签发与校验服务间调用的短时令牌。
//
// notification 需要按租户读取收件人（邮箱），又不能直连 identity 的数据库。
// 这里用一个最小的 HMAC 令牌把租户放进**签名载荷**：identity 只信任签名，
// 租户 ID 不来自 query/path/body，客户端也无法伪造其他租户的令牌。
//
// 它不是用户令牌：没有用户身份、没有角色，只能调用明确标注为 internal 的接口，
// 且有效期只有分钟级。密钥与 JWT 密钥分开配置，泄露面互不影响。
package serviceauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MinSecretLen 与 JWT/回调密钥保持同一强度下限。
const MinSecretLen = 32

// claims 是令牌载荷。字段刻意保持最少：能少给的信息就不放。
type claims struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	ExpiresAt int64     `json:"exp"`
}

// Sign 为指定租户签发一个短期服务令牌。
func Sign(secret string, tenantID uuid.UUID, ttl time.Duration, now time.Time) (string, error) {
	if len(secret) < MinSecretLen {
		return "", fmt.Errorf("serviceauth: 密钥至少需要 %d 字节", MinSecretLen)
	}
	if tenantID == uuid.Nil {
		return "", errors.New("serviceauth: 租户 ID 不能为空")
	}
	if ttl <= 0 {
		return "", errors.New("serviceauth: 有效期必须为正")
	}

	raw, err := json.Marshal(claims{TenantID: tenantID, ExpiresAt: now.Add(ttl).Unix()})
	if err != nil {
		return "", fmt.Errorf("serviceauth: 编码载荷失败: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + sign(secret, payload), nil
}

// Verify 校验签名与有效期，返回令牌声明的租户。
// 错误信息不回显令牌内容，避免把密钥材料带进日志。
func Verify(secret, token string, now time.Time) (uuid.UUID, error) {
	if len(secret) < MinSecretLen {
		return uuid.Nil, fmt.Errorf("serviceauth: 密钥至少需要 %d 字节", MinSecretLen)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return uuid.Nil, errors.New("serviceauth: 令牌格式非法")
	}
	want := sign(secret, parts[0])
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return uuid.Nil, errors.New("serviceauth: 签名不匹配")
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return uuid.Nil, errors.New("serviceauth: 载荷编码非法")
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return uuid.Nil, errors.New("serviceauth: 载荷格式非法")
	}
	if c.TenantID == uuid.Nil || c.ExpiresAt <= 0 {
		return uuid.Nil, errors.New("serviceauth: 载荷缺少必要字段")
	}
	if !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return uuid.Nil, errors.New("serviceauth: 令牌已过期")
	}
	return c.TenantID, nil
}

func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
