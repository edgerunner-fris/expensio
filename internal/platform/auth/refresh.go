package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// refreshTokenBytes = 256 位熵。刷新令牌是长期凭据，长度不足等于给爆破留门。
const refreshTokenBytes = 32

// NewRefreshToken 生成明文令牌与其摘要。
// 明文只在这一次响应里出现，数据库只保存摘要：库被读走也无法冒用。
func NewRefreshToken() (plain string, hash []byte, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("auth: 生成刷新令牌失败: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashRefreshToken(plain), nil
}

// HashRefreshToken 计算查询用的摘要。
// 令牌本身熵足够高，不需要加盐或慢哈希——sha256 即可，且查询要快。
func HashRefreshToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}
