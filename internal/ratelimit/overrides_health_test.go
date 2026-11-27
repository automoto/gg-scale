package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type overrideLoader struct {
	calls int
}

func (l *overrideLoader) APILimit(context.Context, int64) (Limits, bool, error) {
	l.calls++
	return Limits{RatePerSecond: 5, Burst: 10}, true, nil
}

func (l *overrideLoader) InviteLimit(context.Context, int64, int64, string) (Limits, bool, error) {
	return Limits{}, false, nil
}

func TestCachedOverrideStore_should_use_sixty_seconds_when_healthy(t *testing.T) {
	loader := &overrideLoader{}
	cache := NewCachedOverrideStore(loader, DefaultOverrideCacheTTL)
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	cache.SetHealthy(true)
	_, _, err := cache.APILimit(context.Background(), 1)
	require.NoError(t, err)

	now = now.Add(59 * time.Second)
	_, _, _ = cache.APILimit(context.Background(), 1)
	assert.Equal(t, 1, loader.calls)

	now = now.Add(2 * time.Second)
	_, _, _ = cache.APILimit(context.Background(), 1)
	assert.Equal(t, 2, loader.calls)
}

func TestCachedOverrideStore_should_flush_and_use_five_seconds_when_down(t *testing.T) {
	loader := &overrideLoader{}
	cache := NewCachedOverrideStore(loader, DefaultOverrideCacheTTL)
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	cache.SetHealthy(true)
	_, _, _ = cache.APILimit(context.Background(), 1)

	cache.SetHealthy(false)
	_, _, _ = cache.APILimit(context.Background(), 1)
	assert.Equal(t, 2, loader.calls, "the healthy entry must not survive a disconnect")

	now = now.Add(4 * time.Second)
	_, _, _ = cache.APILimit(context.Background(), 1)
	assert.Equal(t, 2, loader.calls)
	now = now.Add(2 * time.Second)
	_, _, _ = cache.APILimit(context.Background(), 1)
	assert.Equal(t, 3, loader.calls)
}

func TestCachedOverrideStore_should_flush_on_reconnect(t *testing.T) {
	loader := &overrideLoader{}
	cache := NewCachedOverrideStore(loader, DefaultOverrideCacheTTL)
	cache.SetHealthy(false)
	_, _, _ = cache.APILimit(context.Background(), 1)

	cache.SetHealthy(true)
	_, _, _ = cache.APILimit(context.Background(), 1)

	assert.Equal(t, 2, loader.calls)
}
