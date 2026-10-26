package money

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want Currency
	}{
		{"大写原样", "USD", "USD"},
		{"小写归一化", "usd", "USD"},
		{"首尾空白", "  eur\t", "EUR"},
		{"零小数币种", "JPY", "JPY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseCurrency(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	invalid := []string{"", "US", "USDD", "U1D", "US$", "人民币", "U S"}
	for _, in := range invalid {
		t.Run("非法_"+in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCurrency(in)
			require.Error(t, err)
			assert.True(t, errors.Is(err, apperr.InvalidArgument("")), "非法币种必须是 400 类错误")
		})
	}
}

func TestCurrencyValid(t *testing.T) {
	t.Parallel()

	assert.True(t, Currency("USD").Valid())
	assert.False(t, Currency("usd").Valid(), "直接构造的小写币种不得通过，避免绕过归一化")
	assert.False(t, Currency("").Valid())
	assert.False(t, Currency("US").Valid())
	assert.False(t, Currency("USDX").Valid())
}

func TestCurrencyExponent(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, Currency("JPY").Exponent())
	assert.Equal(t, 0, Currency("KRW").Exponent())
	assert.Equal(t, 2, Currency("USD").Exponent())
	assert.Equal(t, 2, Currency("CNY").Exponent())
	assert.Equal(t, 3, Currency("KWD").Exponent())
	assert.Equal(t, 2, Currency("XYZ").Exponent(), "未知但格式合法的币种按 2 位小数展示")
}

func TestValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, Validate(MinMinor))
	require.NoError(t, Validate(MaxMinor))

	assert.ErrorIs(t, Validate(0), apperr.InvalidArgument(""))
	assert.ErrorIs(t, Validate(-1), apperr.InvalidArgument(""))
	assert.ErrorIs(t, Validate(MaxMinor+1), apperr.InvalidArgument(""))
	assert.ErrorIs(t, Validate(math.MinInt64), apperr.InvalidArgument(""), "负数取反溢出也不能被放过")
}

func TestValidateAmount(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateAmount(100, "USD"))
	assert.ErrorIs(t, ValidateAmount(0, "USD"), apperr.InvalidArgument(""))
	assert.ErrorIs(t, ValidateAmount(100, "usd"), apperr.InvalidArgument(""),
		"金额校验不负责归一化；调用方必须先 ParseCurrency")
}

func TestFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		minor  int64
		code   Currency
		expect string
	}{
		{"两位小数", 12345, "USD", "123.45 USD"},
		{"不足一元", 5, "USD", "0.05 USD"},
		{"整元", 100, "USD", "1.00 USD"},
		{"零小数", 999, "JPY", "999 JPY"},
		{"三位小数", 1234567, "KWD", "1234.567 KWD"},
		{"上限", MaxMinor, "USD", "10000000000.00 USD"},
		{"负数", -12345, "USD", "-123.45 USD"},
		{"最小负数不溢出", math.MinInt64, "USD", "-92233720368547758.08 USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expect, Format(tc.minor, tc.code))
		})
	}
}
