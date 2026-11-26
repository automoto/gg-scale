package playerauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
)

type epochBackend struct {
	calls int
	epoch int64
	err   error
	inner func()
}

func (b *epochBackend) CurrentEpoch(context.Context, int64) (int64, error) {
	b.calls++
	if b.inner != nil {
		b.inner()
	}
	return b.epoch, b.err
}

func newTestEpochCache(backend EpochValidator, now *time.Time) *CachedEpochValidator {
	cache := NewCachedEpochValidator(backend, 30*time.Second)
	cache.now = func() time.Time { return *now }
	return cache
}

func TestCachedEpochValidator_should_memoize_current_epoch(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{epoch: 3}
	cache := newTestEpochCache(backend, &now)
	ctx := db.WithTenant(context.Background(), 7)

	// Act
	first, err := cache.CurrentEpoch(ctx, 42)
	require.NoError(t, err)
	second, err := cache.CurrentEpoch(ctx, 42)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(3), first)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, backend.calls)
}

func TestCachedEpochValidator_should_reload_after_thirty_seconds(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{epoch: 3}
	cache := newTestEpochCache(backend, &now)
	ctx := db.WithTenant(context.Background(), 7)
	_, _ = cache.CurrentEpoch(ctx, 42)
	now = now.Add(30 * time.Second)

	// Act
	_, err := cache.CurrentEpoch(ctx, 42)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 2, backend.calls)
}

func TestCachedEpochValidator_should_evict_one_player_at_once(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{epoch: 3}
	cache := newTestEpochCache(backend, &now)
	ctx := db.WithTenant(context.Background(), 7)
	_, _ = cache.CurrentEpoch(ctx, 42)

	// Act
	cache.Evict(42)
	_, err := cache.CurrentEpoch(ctx, 42)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 2, backend.calls)
}

func TestCachedEpochValidator_should_evict_only_the_selected_tenant(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{epoch: 3}
	cache := newTestEpochCache(backend, &now)
	_, _ = cache.CurrentEpoch(db.WithTenant(context.Background(), 7), 42)
	_, _ = cache.CurrentEpoch(db.WithTenant(context.Background(), 8), 43)

	// Act
	cache.EvictTenant(7)
	_, _ = cache.CurrentEpoch(db.WithTenant(context.Background(), 7), 42)
	_, err := cache.CurrentEpoch(db.WithTenant(context.Background(), 8), 43)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 3, backend.calls)
}

func TestCachedEpochValidator_should_not_store_a_read_that_raced_with_eviction(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{epoch: 3}
	cache := newTestEpochCache(backend, &now)
	backend.inner = func() { cache.Evict(42) }
	ctx := db.WithTenant(context.Background(), 7)
	_, _ = cache.CurrentEpoch(ctx, 42)

	// Act
	_, err := cache.CurrentEpoch(ctx, 42)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 2, backend.calls)
}

func TestCachedEpochValidator_should_not_cache_backend_errors(t *testing.T) {
	// Arrange
	now := time.Unix(1000, 0)
	backend := &epochBackend{err: errors.New("database unavailable")}
	cache := newTestEpochCache(backend, &now)
	ctx := db.WithTenant(context.Background(), 7)

	// Act
	_, firstErr := cache.CurrentEpoch(ctx, 42)
	_, secondErr := cache.CurrentEpoch(ctx, 42)

	// Assert
	require.Error(t, firstErr)
	require.Error(t, secondErr)
	assert.Equal(t, 2, backend.calls)
}
