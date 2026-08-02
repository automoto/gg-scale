// Package period computes leaderboard period reset boundaries. Boundaries
// align to the UTC calendar so every deployment resets at the same wall-clock
// moment: daily at midnight, weekly on Monday 00:00, monthly on the 1st
// 00:00. The reset job runs a few minutes behind a boundary at worst; the
// boundary itself, not the job's run time, is recorded as the period edge.
package period

import "time"

// Reset schedules a leaderboard can carry. They mirror the DB CHECK
// constraint on leaderboards.reset_schedule.
const (
	ScheduleNone    = "none"
	ScheduleDaily   = "daily"
	ScheduleWeekly  = "weekly"
	ScheduleMonthly = "monthly"
)

// ValidSchedule reports whether s is one of the known schedules.
func ValidSchedule(s string) bool {
	switch s {
	case ScheduleNone, ScheduleDaily, ScheduleWeekly, ScheduleMonthly:
		return true
	}
	return false
}

// NextReset returns the first boundary of schedule strictly after from.
// ok is false when the schedule has no boundaries (none or unknown).
func NextReset(schedule string, from time.Time) (time.Time, bool) {
	u := from.UTC()
	midnight := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	switch schedule {
	case ScheduleDaily:
		return midnight.AddDate(0, 0, 1), true
	case ScheduleWeekly:
		days := (int(time.Monday) - int(midnight.Weekday()) + 7) % 7
		next := midnight.AddDate(0, 0, days)
		if !next.After(u) {
			next = next.AddDate(0, 0, 7)
		}
		return next, true
	case ScheduleMonthly:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0), true
	}
	return time.Time{}, false
}
