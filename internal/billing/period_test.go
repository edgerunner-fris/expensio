package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func utcDate(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestPeriodEnd(t *testing.T) {
	tests := []struct {
		name     string
		start    time.Time
		interval PlanInterval
		want     time.Time
	}{
		{"月付普通日期", utcDate(2026, 10, 1), IntervalMonthly, utcDate(2026, 10, 31)},
		{"月付月中", utcDate(2026, 10, 15), IntervalMonthly, utcDate(2026, 11, 14)},
		{"月付跨年", utcDate(2026, 12, 15), IntervalMonthly, utcDate(2027, 1, 14)},
		{"月付 31 号收敛到 2 月末", utcDate(2026, 1, 31), IntervalMonthly, utcDate(2026, 2, 27)},
		{"月付闰年 2 月", utcDate(2024, 1, 31), IntervalMonthly, utcDate(2024, 2, 28)},
		{"年付普通日期", utcDate(2026, 10, 15), IntervalYearly, utcDate(2027, 10, 14)},
		{"年付闰日收敛", utcDate(2024, 2, 29), IntervalYearly, utcDate(2025, 2, 27)},
		{"年付 12 月", utcDate(2026, 12, 31), IntervalYearly, utcDate(2027, 12, 30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PeriodEnd(tt.start, tt.interval)
			require.NoError(t, err)
			assert.True(t, got.Equal(tt.want), "got %s want %s", got, tt.want)
		})
	}
}

func TestPeriodEndIsDateOnly(t *testing.T) {
	// 带时区与时分秒的输入先归一到 UTC 日期，输出总是 00:00:00 UTC。
	start := time.Date(2026, 10, 15, 23, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))
	got, err := PeriodEnd(start, IntervalMonthly)
	require.NoError(t, err)
	assert.True(t, got.Equal(utcDate(2026, 11, 14)), "got %s", got)
	assert.Equal(t, time.UTC, got.Location())
	assert.Equal(t, 0, got.Hour())
	assert.Equal(t, 0, got.Minute())
}

func TestNextPeriodStartKeepsAnchorDay(t *testing.T) {
	tests := []struct {
		name     string
		start    time.Time
		interval PlanInterval
		anchor   int
		want     time.Time
	}{
		{"月付月中", utcDate(2026, 10, 15), IntervalMonthly, 15, utcDate(2026, 11, 15)},
		{"31 日锚点在 2 月收敛", utcDate(2026, 1, 31), IntervalMonthly, 31, utcDate(2026, 2, 28)},
		{"短月之后恢复 31 日", utcDate(2026, 2, 28), IntervalMonthly, 31, utcDate(2026, 3, 31)},
		{"4 月只有 30 天", utcDate(2026, 3, 31), IntervalMonthly, 31, utcDate(2026, 4, 30)},
		{"跨年", utcDate(2026, 12, 15), IntervalMonthly, 15, utcDate(2027, 1, 15)},
		{"年付闰日收敛到 2 月末", utcDate(2024, 2, 29), IntervalYearly, 29, utcDate(2025, 2, 28)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NextPeriodStart(tt.start, tt.interval, tt.anchor)
			require.NoError(t, err)
			assert.True(t, got.Equal(tt.want), "got %s want %s", got, tt.want)
		})
	}
}

// 续费的硬性前提：新周期必须紧接旧周期。周期结束日 + 1 必须等于
// NextPeriodStart 的结果，否则 guard 会拒绝整条更新。
func TestNextPeriodStartIsContiguousWithPeriodEnd(t *testing.T) {
	for _, start := range []time.Time{
		utcDate(2026, 1, 31),
		utcDate(2026, 2, 28),
		utcDate(2026, 3, 31),
		utcDate(2026, 10, 15),
		utcDate(2024, 2, 29),
	} {
		for _, interval := range []PlanInterval{IntervalMonthly, IntervalYearly} {
			end, err := PeriodEnd(start, interval)
			require.NoError(t, err)
			next, err := NextPeriodStart(start, interval, start.Day())
			require.NoError(t, err)
			assert.True(t, next.Equal(end.AddDate(0, 0, 1)),
				"start=%s interval=%s end=%s next=%s", start, interval, end, next)
		}
	}
}

func TestNextPeriodStartRejectsInvalidAnchor(t *testing.T) {
	_, err := NextPeriodStart(utcDate(2026, 10, 1), IntervalMonthly, 0)
	requireInvalidArgument(t, err)
	_, err = NextPeriodStart(utcDate(2026, 10, 1), IntervalMonthly, 32)
	requireInvalidArgument(t, err)
	_, err = NextPeriodStart(utcDate(2026, 10, 1), PlanInterval("weekly"), 1)
	requireInvalidArgument(t, err)
}

func TestPeriodEndRejectsUnknownInterval(t *testing.T) {
	_, err := PeriodEnd(utcDate(2026, 10, 1), PlanInterval("weekly"))
	requireInvalidArgument(t, err)
	_, err = PeriodEnd(utcDate(2026, 10, 1), PlanInterval(""))
	requireInvalidArgument(t, err)
}
