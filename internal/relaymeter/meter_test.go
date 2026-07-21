package relaymeter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMonthStart_truncates_to_first_of_month_utc(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 7, 18, 15, 4, 5, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		// Non-UTC wall time normalizes to the UTC month.
		{time.Date(2026, 8, 1, 5, 0, 0, 0, time.FixedZone("plus9", 9*3600)), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, monthStart(tc.in), "in=%s", tc.in)
	}
}

func TestWarnThresholds(t *testing.T) {
	assert.True(t, crossed80(800, 1000))
	assert.False(t, crossed80(799, 1000))
	assert.True(t, crossed100(1000, 1000))
	assert.False(t, crossed100(999, 1000))
}
