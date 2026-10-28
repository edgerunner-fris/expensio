package identity

import (
	"strings"
	"unicode/utf8"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

// 输入归一化与通用错误构造。集中在一处，保证同样的输入在所有入口得到同样的判定。

func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) > maxEmailLen {
		return "", apperr.InvalidArgument("邮箱过长")
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return "", apperr.InvalidArgument("邮箱格式非法")
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return "", apperr.InvalidArgument("邮箱格式非法")
	}
	return email, nil
}

func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return apperr.InvalidArgument("密码至少 10 个字符")
	}
	if len(pw) > maxPasswordLen {
		return apperr.InvalidArgument("密码不超过 128 个字符")
	}
	return nil
}

// validateDisplayName 去首尾空白后校验：非空、不超过 64 个字符、不含控制字符。
// 展示名会进入通知邮件、审计与页面，允许换行或控制字符等于给下游埋注入点。
func validateDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apperr.InvalidArgument("显示名称不能为空")
	}
	if utf8.RuneCountInString(name) > maxDisplayLen {
		return "", apperr.InvalidArgument("显示名称不超过 64 个字符")
	}
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", apperr.InvalidArgument("显示名称包含非法字符")
	}
	return name, nil
}

func errInvalidCredentials() *apperr.Error {
	return apperr.Unauthorized("邮箱或密码错误")
}

func errSessionExpired() *apperr.Error {
	return apperr.Unauthorized("登录状态已失效，请重新登录")
}
