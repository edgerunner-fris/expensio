package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// KeyID 按 RFC 7638 计算 Ed25519 公钥指纹，作为 JWT header 的 kid。
// 用指纹而不是自增编号，轮换时不会因为两份配置对同一个编号
// 指向不同密钥而验签失败。
func KeyID(pub ed25519.PublicKey) string {
	thumbprint := fmt.Sprintf(`{"crv":"Ed25519","kty":"OKP","x":%q}`,
		base64.RawURLEncoding.EncodeToString(pub))
	sum := sha256.Sum256([]byte(thumbprint))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// JWK / JWKS 是 /.well-known/jwks.json 的响应结构。
// 只暴露公钥；私钥永远不离开 identity 的进程内存与 Secret。
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKS 返回当前所有可验签的公钥，按 kid 排序保证输出稳定（便于缓存与比对）。
func (i *TokenIssuer) JWKS() JWKS {
	keys := make([]JWK, 0, len(i.verifyKeys))
	for kid, pub := range i.verifyKeys {
		keys = append(keys, JWK{
			Kty: "OKP",
			Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(pub),
			Kid: kid,
			Use: "sig",
			Alg: "EdDSA",
		})
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a].Kid < keys[b].Kid })
	return JWKS{Keys: keys}
}

// ParsePrivateKey 解析 base64 编码的 Ed25519 私钥（32 字节种子或 64 字节私钥）。
func ParsePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := decodeBase64(encoded)
	if err != nil {
		return nil, fmt.Errorf("auth: 私钥不是合法的 base64: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("auth: Ed25519 私钥长度非法: %d", len(raw))
	}
}

// ParsePublicKeys 解析逗号分隔的 base64 Ed25519 公钥列表。
// 支持多个公钥是为了轮换：新私钥上线后，旧公钥继续接受
// 直到此前签出的令牌全部过期。
func ParsePublicKeys(csv string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	parts := strings.Split(csv, ",")
	keys := make([]ed25519.PublicKey, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		raw, err := decodeBase64(part)
		if err != nil {
			return nil, fmt.Errorf("auth: 公钥不是合法的 base64: %w", err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("auth: Ed25519 公钥长度非法: %d", len(raw))
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// decodeBase64 接受标准/URL 安全、带或不带填充的写法：
// 不同工具生成的 .env 不应因为一个 '=' 就让服务起不来。
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	var lastErr error
	for _, enc := range encodings {
		raw, err := enc.DecodeString(s)
		if err == nil {
			return raw, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
