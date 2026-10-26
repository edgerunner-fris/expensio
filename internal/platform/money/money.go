// Package money 用 int64 最小货币单位（minor units）表示金额。
//
// 为什么不用 float64：0.1 + 0.2 != 0.3 在财务系统里是事故，不是精度问题；
// JSON 里的浮点还可能被前端再次舍入。这里传到前端、存进数据库的都是整数，
// 只有展示层才把它格式化成小数点形式。
//
// 为什么不引入 decimal 库：本项目对金额只做"比较、相加、条件更新"，
// 这些都是整数运算；数据库用 bigint 存，SQL 里做加减，精度天然对齐。
// 真需要按比例分摊时再评估 decimal，而不是现在就为假设的需求付复杂度。
package money

import (
	"fmt"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

const (
	// MaxMinor 与数据库 CHECK（amount_minor <= 1000000000000）一致，
	// 即 1e12 个最小单位（USD 下为一百亿美元）。放在这里是为了让
	// 非法金额在进入事务前就被拒绝，而不是等数据库报 23514。
	MaxMinor int64 = 1_000_000_000_000
	// MinMinor 与数据库 CHECK（amount_minor > 0）一致：零金额不是一张费用单。
	MinMinor int64 = 1
)

// Currency 是 ISO-4217 的三位字母代码，始终以大写形式在系统内传递。
type Currency string

// zeroExponent 是小数位为 0 的币种（日元、韩元等）。
// 不在此表、也不在 threeExponent 的币种按 2 位小数处理——这是 ISO-4217 的绝大多数。
var zeroExponent = map[Currency]struct{}{
	"BIF": {}, "CLP": {}, "DJF": {}, "GNF": {}, "ISK": {}, "JPY": {},
	"KMF": {}, "KRW": {}, "PYG": {}, "RWF": {}, "UGX": {}, "UYI": {},
	"VND": {}, "VUV": {}, "XAF": {}, "XOF": {}, "XPF": {},
}

// threeExponent 是小数位为 3 的币种（海湾国家第纳尔等）。
var threeExponent = map[Currency]struct{}{
	"BHD": {}, "IQD": {}, "JOD": {}, "KWD": {}, "LYD": {}, "OMR": {}, "TND": {},
}

// ParseCurrency 归一化并校验币种代码：去空白、转大写、必须是 3 个 ASCII 字母。
//
// 归一化而不是原样接受小写，是为了让数据库里只有一种写法；
// 数据库仍保留 CHECK (currency ~ '^[A-Z]{3}$') 作为最后一道防线，
// 这里做校验只是为了让错误在 HTTP 层就变成 400，而不是 500。
func ParseCurrency(raw string) (Currency, error) {
	code := Currency(normalize(raw))
	if !code.Valid() {
		return "", apperr.InvalidArgument("币种必须是 3 位字母代码，例如 USD")
	}
	return code, nil
}

// Valid 判断是否为 3 位 ASCII 大写字母。空串与 Unicode 字母都不通过。
func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return false
		}
	}
	return true
}

func (c Currency) String() string { return string(c) }

// Exponent 返回该币种的小数位数，仅用于展示，不参与计算。
// 未知但格式合法的币种默认 2 位。
func (c Currency) Exponent() int {
	if _, ok := zeroExponent[c]; ok {
		return 0
	}
	if _, ok := threeExponent[c]; ok {
		return 3
	}
	return 2
}

// Validate 校验最小单位金额是否落在数据库允许的范围内。
func Validate(minor int64) error {
	if minor < MinMinor {
		return apperr.InvalidArgument("金额必须大于 0")
	}
	if minor > MaxMinor {
		return apperr.InvalidArgument("金额超出允许范围")
	}
	return nil
}

// ValidateAmount 同时校验金额与币种，用于所有写入路径的入口。
func ValidateAmount(minor int64, currency Currency) error {
	if err := Validate(minor); err != nil {
		return err
	}
	if !currency.Valid() {
		return apperr.InvalidArgument("币种必须是 3 位字母代码，例如 USD")
	}
	return nil
}

// Format 把最小单位金额转成展示字符串，例如 (12345, "USD") -> "123.45 USD"。
//
// 只用于日志、通知与报表文案；接口响应仍然返回整数 amount_minor + currency，
// 由前端负责本地化展示。这样服务端永远不会把金额变成浮点数。
func Format(minor int64, c Currency) string {
	sign := ""
	// 处理 MinInt64：直接取负会溢出，先加一取负再补一。
	var v uint64
	if minor < 0 {
		sign = "-"
		v = uint64(-(minor + 1)) + 1
	} else {
		v = uint64(minor)
	}

	exp := c.Exponent()
	if exp == 0 {
		return fmt.Sprintf("%s%d %s", sign, v, c)
	}
	pow := uint64(1)
	for i := 0; i < exp; i++ {
		pow *= 10
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, v/pow, exp, v%pow, c)
}

// normalize 去掉首尾空白并转成大写。只处理 ASCII，避免 ToUpper 把某些
// Unicode 字符映射成多字节结果、长度恰好变成 3 的怪事。
func normalize(raw string) string {
	start, end := 0, len(raw)
	for start < end && isSpace(raw[start]) {
		start++
	}
	for end > start && isSpace(raw[end-1]) {
		end--
	}
	b := make([]byte, 0, end-start)
	for i := start; i < end; i++ {
		c := raw[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
