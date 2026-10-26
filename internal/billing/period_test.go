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

func TestPeriodEndRejectsUnknownInterval(t *testing.T) {
	_, err := PeriodEnd(utcDate(2026, 10, 1), PlanInterval("weekly"))
	requireInvalidArgument(t, err)
	_, err = PeriodEnd(utcDate(2026, 10, 1), PlanInterval(""))
	requireInvalidArgument(t, err)
}
