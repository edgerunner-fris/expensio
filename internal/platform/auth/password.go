package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数：64 MiB / 3 轮 / 并行 2。
// 这是 OWASP 推荐的偏低配档位，单次校验约 50ms——足以让离线爆破昂贵，
// 又不会在登录高峰把 CPU 打满。调参必须同时改这里和 TestPasswordHashCost。
const (
	argonMemory  uint32 = 64 * 1024
	argonTime    uint32 = 3
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	saltLength          = 16

	// 校验时对参数设上限：数据库里的哈希若被篡改成 m=4GiB，
	// 一次登录就能把进程内存打爆。宁可判为非法哈希。
	maxArgonMemory  uint32 = 256 * 1024
	maxArgonTime    uint32 = 10
	maxArgonThreads uint8  = 8
)

// ErrInvalidHash 表示存储的哈希不是本系统能解析的格式。
var ErrInvalidHash = errors.New("auth: 密码哈希格式非法")

// HashPassword 生成 PHC 字符串：$argon2id$v=19$m=...,t=...,p=...$salt$key
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 用常量时间比较，避免通过响应时间探测前缀匹配长度。
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func parseHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// 前导 $ 会产生一个空串，所以 6 段对应 5 个实际字段。
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, ErrInvalidHash
	}

	var p argonParams
	var m, t, threads uint64
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &threads); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	if m == 0 || m > uint64(maxArgonMemory) ||
		t == 0 || t > uint64(maxArgonTime) ||
		threads == 0 || threads > uint64(maxArgonThreads) {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	p.memory, p.time, p.threads = uint32(m), uint32(t), uint8(threads)

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
