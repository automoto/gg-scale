package matchmaker

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNotifyLimiterAllow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	t.Run("should_allow_first_send_for_a_key", func(t *testing.T) {
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return base }

		assert.True(t, l.allow("bucket-a"))
	})

	t.Run("should_debounce_second_send_within_interval", func(t *testing.T) {
		now := base
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return now }

		assert.True(t, l.allow("bucket-a"))
		now = base.Add(50 * time.Millisecond)
		assert.False(t, l.allow("bucket-a"))
	})

	t.Run("should_allow_again_after_interval_elapses", func(t *testing.T) {
		now := base
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return now }

		assert.True(t, l.allow("bucket-a"))
		now = base.Add(100 * time.Millisecond)
		assert.True(t, l.allow("bucket-a"))
	})

	t.Run("should_track_keys_independently", func(t *testing.T) {
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return base }

		assert.True(t, l.allow("bucket-a"))
		assert.True(t, l.allow("bucket-b"))
		assert.False(t, l.allow("bucket-a"))
	})
}

func TestNotifyLimiterForget(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	t.Run("should_allow_again_immediately_after_forget", func(t *testing.T) {
		now := base
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return now }

		assert.True(t, l.allow("bucket-a"))
		l.forget("bucket-a")
		now = base.Add(50 * time.Millisecond)
		assert.True(t, l.allow("bucket-a"))
	})

	t.Run("should_ignore_unknown_key", func(t *testing.T) {
		l := newNotifyLimiter(100 * time.Millisecond)
		l.now = func() time.Time { return base }

		assert.True(t, l.allow("bucket-a"))
		l.forget("bucket-b")
		assert.False(t, l.allow("bucket-a"))
	})
}

func TestNotifyLimiterPrunesStaleKeys(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newNotifyLimiter(100 * time.Millisecond)
	l.now = func() time.Time { return now }

	// Fill past the prune threshold with keys that all go stale.
	for i := 0; i < notifyLimiterPruneThreshold; i++ {
		assert.True(t, l.allow("stale-"+strconv.Itoa(i)))
	}
	// Advance beyond the interval so the existing keys are prunable, then a
	// fresh send trips the prune and reclaims them.
	now = now.Add(time.Second)
	assert.True(t, l.allow("fresh"))
	assert.Equal(t, 1, l.size())
}
