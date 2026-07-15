package olric

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInt64Codec_preserves_signed_bit_pattern(t *testing.T) {
	for _, want := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		raw := make([]byte, 8)

		putInt64(raw, want)
		got := readInt64(raw)

		assert.Equal(t, want, got)
	}
}
