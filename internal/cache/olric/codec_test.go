package olric

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
)

func TestInt64Codec_preserves_signed_bit_pattern(t *testing.T) {
	for _, want := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		raw := make([]byte, 8)

		putInt64(raw, want)
		got := readInt64(raw)

		assert.Equal(t, want, got)
	}
}

func TestBurstCodec_round_trips_signed_durations_and_timestamps(t *testing.T) {
	want := cache.BurstSlotState{
		Count:          17,
		BurstRemaining: -3*time.Second - 41*time.Nanosecond,
		LastAssessed:   time.Unix(-123, 456789),
		Expires:        time.Unix(1_900_000_000, 987654321),
		Sustained:      50_000,
		BurstBudget:    -time.Duration(math.MaxInt64),
	}

	raw := encodeBurst(want)
	got, ok := decodeBurst(raw)

	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestBurstCodec_accepts_legacy_four_field_state(t *testing.T) {
	want := cache.BurstSlotState{
		Count:          4,
		BurstRemaining: 5 * time.Minute,
		LastAssessed:   time.Unix(1_700_000_000, 0),
		Expires:        time.Unix(1_700_000_060, 0),
	}

	raw := encodeBurst(want)[:32]
	got, ok := decodeBurst(raw)

	require.True(t, ok)
	assert.Equal(t, want, got)
}
