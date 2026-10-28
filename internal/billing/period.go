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
// 输入按 UTC 日期解释；调用方应传日粒度的日期。
func PeriodEnd(start time.Time, interval PlanInterval) (time.Time, error) {
	from := dateOnly(start)
	next, err := NextPeriodStart(from, interval, from.Day())
	if err != nil {
		return time.Time{}, err
	}
	return next.AddDate(0, 0, -1), nil
}

// NextPeriodStart 返回紧接当前周期之后的下一个周期的开始日。
//
// 推进按订阅的"计费锚点日"（anchorDay）而不是当前开始日：1 月 31 日开通的
// 月付订阅，2 月周期结束于 27 日，3 月仍从 31 日开始（目标月份没有 31 日时
// 收敛到月末）。没有锚点，日期会逐月漂移成 28 日、29 日……。
//
// anchorDay 由订阅创建时确定并写入 subscriptions.anchor_day，之后不可变。
func NextPeriodStart(currentStart time.Time, interval PlanInterval, anchorDay int) (time.Time, error) {
	if anchorDay < 1 || anchorDay > 31 {
		return time.Time{}, apperr.InvalidArgument("计费锚点日必须在 1 到 31 之间")
	}
	months, err := intervalMonths(interval)
	if err != nil {
		return time.Time{}, err
	}
	from := dateOnly(currentStart)
	target := time.Date(from.Year(), from.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	day := anchorDay
	if last := daysInMonth(target.Year(), target.Month()); day > last {
		day = last
	}
	return time.Date(target.Year(), target.Month(), day, 0, 0, 0, 0, time.UTC), nil
}

func intervalMonths(interval PlanInterval) (int, error) {
	switch interval {
	case IntervalMonthly:
		return 1, nil
	case IntervalYearly:
		return 12, nil
	default:
		return 0, apperr.InvalidArgument("套餐周期必须是 monthly 或 yearly")
	}
}

func dateOnly(t time.Time) time.Time {
	year, month, day := t.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
