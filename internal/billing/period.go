package billing

import (
	"time"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

// PeriodEnd 返回从 start 开始的一个计费周期的最后一天（含）。
//
// 周期按"周年"推进：下周期的开始日 = 本周期开始日 + 1 个月/1 年，
// 遇到目标月份没有该日期时收敛到当月最后一天（1 月 31 日的下一个月是 2 月 28 日，
// 本周期结束于 2 月 27 日）。
//
// 输入按 UTC 日期解释；调用方应传日粒度的日期。M4 不做续费，
// 因此不需要在订阅上额外保存"计费锚点日"；引入续费时再评估 anchor day。
func PeriodEnd(start time.Time, interval PlanInterval) (time.Time, error) {
	from := dateOnly(start)
	switch interval {
	case IntervalMonthly:
		return addMonthsClamped(from, 1).AddDate(0, 0, -1), nil
	case IntervalYearly:
		return addMonthsClamped(from, 12).AddDate(0, 0, -1), nil
	default:
		return time.Time{}, apperr.InvalidArgument("套餐周期必须是 monthly 或 yearly")
	}
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// addMonthsClamped 把日期推进 N 个月，日超出目标月份天数时收敛到月末。
func addMonthsClamped(t time.Time, months int) time.Time {
	year, month, day := t.Date()
	first := time.Date(year, month+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := daysInMonth(first.Year(), first.Month())
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
