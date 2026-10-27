package config

import (
	"fmt"

	"github.com/edgerunner-fris/expensio/internal/platform/auth"
)

// AuthIssuer 把字符串形式的 JWT 配置解析成签发/校验器。
//
// 解析失败在启动期返回：密钥格式错误不应该等到第一个请求 401 才暴露。
// 这里集中处理 base64 解码，避免六个 main 各写一遍、某一处漏掉长度校验。
func (j JWT) AuthIssuer() (*auth.TokenIssuer, error) {
	cfg := auth.IssuerConfig{
		Issuer:     j.Issuer,
		AccessTTL:  j.AccessTTL,
		RefreshTTL: j.RefreshTTL,
	}
	if j.PrivateKey != "" {
		key, err := auth.ParsePrivateKey(j.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("config: 解析 jwt.private_key 失败: %w", err)
		}
		cfg.PrivateKey = key
	}
	publicKeys, err := auth.ParsePublicKeys(j.PublicKeys)
	if err != nil {
		return nil, fmt.Errorf("config: 解析 jwt.public_keys 失败: %w", err)
	}
	cfg.PublicKeys = publicKeys

	issuer, err := auth.NewTokenIssuer(cfg)
	if err != nil {
		return nil, fmt.Errorf("config: 构建令牌签发器失败: %w", err)
	}
	return issuer, nil
}
