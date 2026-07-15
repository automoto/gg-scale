package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStorageWarnNotify_without_mailer_is_not_delivered(t *testing.T) {
	w := &StorageWarnWorker{}

	err := w.notify(context.Background(), 1, "tenant", 80, 100, 80)

	assert.Error(t, err)
}

func TestStorageThreshold_crossings(t *testing.T) {
	const gb = int64(1) << 30
	limit := 5 * gb
	cases := []struct {
		name  string
		total int64
		want  int16
	}{
		{"empty", 0, 0},
		{"half", 2 * gb, 0},
		{"just_under_80", 3*gb + 900*1024*1024, 0},
		{"at_80", 4 * gb, 80},
		{"between_80_and_100", 45 * gb / 10, 80},
		{"at_100", 5 * gb, 100},
		{"over_100", 6 * gb, 100},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, storageThreshold(tc.total, limit), tc.name)
	}
}

func TestStorageThreshold_unlimited_or_unknown_never_warns(t *testing.T) {
	assert.Equal(t, int16(0), storageThreshold(1<<40, -1), "unlimited")
	assert.Equal(t, int16(0), storageThreshold(1<<40, 0), "zero limit")
}

func TestHumanizeBytes(t *testing.T) {
	const gb = int64(1) << 30
	assert.Equal(t, "5.0 GB", humanizeBytes(5*gb))
	assert.Equal(t, "1.5 GB", humanizeBytes(gb+gb/2))
	assert.Equal(t, "512.0 KB", humanizeBytes(512*1024))
	assert.Equal(t, "0 B", humanizeBytes(0))
}
