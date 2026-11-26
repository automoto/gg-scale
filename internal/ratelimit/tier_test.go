package ratelimit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/ratelimit"
)

func TestScaleLimits_should_scale_rate_and_burst_by_instance_share(t *testing.T) {
	// Arrange
	combined := ratelimit.Limits{RatePerSecond: 1000, Burst: 10000}

	// Act
	got := ratelimit.ScaleLimits(combined, 0.5)

	// Assert
	assert.Equal(t, ratelimit.Limits{RatePerSecond: 500, Burst: 5000}, got)
}

func TestScaleLimits_should_leave_combined_limits_at_default_share(t *testing.T) {
	// Arrange
	combined := ratelimit.Limits{RatePerSecond: 250, Burst: 2500}

	// Act
	got := ratelimit.ScaleLimits(combined, 1)

	// Assert
	assert.Equal(t, combined, got)
}
