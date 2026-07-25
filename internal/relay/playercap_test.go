package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPlayerAllocLimiterThrottlesFloodPerPlayer(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(6, 3) // burst 3
	lim.now = func() time.Time { return now }

	// The burst is consumed, then the flood is throttled...
	assert.True(t, lim.allow(1, 100))
	assert.True(t, lim.allow(1, 100))
	assert.True(t, lim.allow(1, 100))
	assert.False(t, lim.allow(1, 100), "4th op in the same instant is throttled")

	// ...but a different player is unaffected (per-player, not global).
	assert.True(t, lim.allow(1, 200))

	// The same player-id under a different tenant is a distinct bucket.
	assert.True(t, lim.allow(2, 100))

	// Tokens refill over time: 6/min = 1 per 10s.
	now = now.Add(10 * time.Second)
	assert.True(t, lim.allow(1, 100), "a token refills after 10s")
	assert.False(t, lim.allow(1, 100))
}

func TestPlayerAllocLimiterDisabled(t *testing.T) {
	assert.Nil(t, newPlayerAllocLimiter(0, 20), "zero rate disables")
	assert.Nil(t, newPlayerAllocLimiter(6, 0), "zero burst disables")

	var lim *playerAllocLimiter
	for i := 0; i < 100; i++ {
		assert.True(t, lim.allow(1, 100), "a nil limiter never throttles")
	}
}
