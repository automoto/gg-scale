package players

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGraceLabel_renders_configured_duration_without_truncation(t *testing.T) {
	cases := []struct {
		name  string
		grace time.Duration
		want  string
	}{
		{name: "should_render_default_as_days", grace: 720 * time.Hour, want: "30 days"},
		{name: "should_render_single_day", grace: 24 * time.Hour, want: "1 day"},
		{name: "should_render_uneven_hours_as_hours", grace: 36 * time.Hour, want: "36 hours"},
		{name: "should_render_single_hour", grace: time.Hour, want: "1 hour"},
		{name: "should_render_sub_hour_exactly", grace: 90 * time.Minute, want: "1h30m0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, graceLabel(c.grace))
		})
	}
}

func TestPurgeTimeLabel_should_show_time_in_utc(t *testing.T) {
	at := time.Date(2026, 10, 5, 14, 30, 0, 0, time.FixedZone("EST", -5*3600))

	assert.Equal(t, "2026-10-05 19:30 UTC", purgeTimeLabel(at))
}
