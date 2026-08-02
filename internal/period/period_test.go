package period

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func at(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func TestNextReset_boundaries(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		from     time.Time
		want     time.Time
		ok       bool
	}{
		{"daily_midday", ScheduleDaily, at(2026, 7, 15, 13, 45), at(2026, 7, 16, 0, 0), true},
		{"daily_exact_boundary_moves_forward", ScheduleDaily, at(2026, 7, 15, 0, 0), at(2026, 7, 16, 0, 0), true},
		{"daily_year_rollover", ScheduleDaily, at(2026, 12, 31, 23, 59), at(2027, 1, 1, 0, 0), true},
		// 2026-07-15 is a Wednesday; the next Monday is 2026-07-20.
		{"weekly_midweek", ScheduleWeekly, at(2026, 7, 15, 13, 45), at(2026, 7, 20, 0, 0), true},
		// 2026-07-20 is a Monday; exactly at the boundary jumps a full week.
		{"weekly_exact_boundary_moves_forward", ScheduleWeekly, at(2026, 7, 20, 0, 0), at(2026, 7, 27, 0, 0), true},
		// 2026-07-19 is a Sunday.
		{"weekly_sunday", ScheduleWeekly, at(2026, 7, 19, 23, 0), at(2026, 7, 20, 0, 0), true},
		{"monthly_midmonth", ScheduleMonthly, at(2026, 7, 15, 13, 45), at(2026, 8, 1, 0, 0), true},
		{"monthly_exact_boundary_moves_forward", ScheduleMonthly, at(2026, 8, 1, 0, 0), at(2026, 9, 1, 0, 0), true},
		{"monthly_year_rollover", ScheduleMonthly, at(2026, 12, 15, 0, 0), at(2027, 1, 1, 0, 0), true},
		{"none_has_no_boundary", ScheduleNone, at(2026, 7, 15, 13, 45), time.Time{}, false},
		{"unknown_has_no_boundary", "hourly", at(2026, 7, 15, 13, 45), time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := NextReset(c.schedule, c.from)
			assert.Equal(t, c.ok, ok)
			if c.ok {
				assert.True(t, got.Equal(c.want), "got %v want %v", got, c.want)
				assert.True(t, got.After(c.from), "boundary must be strictly after from")
			}
		})
	}
}

func TestNextReset_normalizes_non_utc_input(t *testing.T) {
	// 2026-07-15 22:00 -0300 is 2026-07-16 01:00 UTC, so the next daily
	// boundary is the 17th, not the 16th.
	loc := time.FixedZone("BRT", -3*3600)
	got, ok := NextReset(ScheduleDaily, time.Date(2026, 7, 15, 22, 0, 0, 0, loc))
	assert.True(t, ok)
	assert.True(t, got.Equal(at(2026, 7, 17, 0, 0)), "got %v", got)
}

func TestValidSchedule(t *testing.T) {
	for _, s := range []string{ScheduleNone, ScheduleDaily, ScheduleWeekly, ScheduleMonthly} {
		assert.True(t, ValidSchedule(s), s)
	}
	for _, s := range []string{"", "hourly", "Daily", "yearly"} {
		assert.False(t, ValidSchedule(s), s)
	}
}
