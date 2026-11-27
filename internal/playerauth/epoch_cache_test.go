package playerauth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type epochLoader struct {
	mu     sync.Mutex
	epoch  int64
	calls  int
	err    error
	start  chan struct{}
	resume chan struct{}
}

func (l *epochLoader) CurrentEpoch(context.Context, int64) (int64, error) {
	l.mu.Lock()
	l.calls++
	call := l.calls
	epoch, err := l.epoch, l.err
	start, resume := l.start, l.resume
	l.mu.Unlock()
	if call == 1 && start != nil {
		close(start)
		<-resume
	}
	return epoch, err
}

func (l *epochLoader) setEpoch(epoch int64) {
	l.mu.Lock()
	l.epoch = epoch
	l.mu.Unlock()
}

func (l *epochLoader) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func healthyEpochCache(loader EpochValidator, ttl time.Duration) *CachedEpochValidator {
	cache := NewCachedEpochValidator(loader, ttl, nil)
	cache.SetHealthy(true)
	return cache
}

func TestCachedEpochValidator_should_reuse_a_matching_epoch(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, time.Minute)

	for range 3 {
		epoch, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
		require.NoError(t, err)
		assert.Equal(t, int64(4), epoch)
	}

	assert.Equal(t, 1, loader.callCount())
}

func TestCachedEpochValidator_should_reload_when_a_token_has_a_higher_epoch(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, time.Minute)
	_, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)
	loader.setEpoch(5)

	epoch, err := cache.CurrentEpochForToken(context.Background(), 42, 5)

	require.NoError(t, err)
	assert.Equal(t, int64(5), epoch)
	assert.Equal(t, 2, loader.callCount())
}

func TestCachedEpochValidator_should_reject_a_newer_cached_epoch_without_reloading(t *testing.T) {
	loader := &epochLoader{epoch: 6}
	cache := healthyEpochCache(loader, time.Minute)

	_, err := cache.CurrentEpochForToken(context.Background(), 42, 5)
	require.NoError(t, err)
	epoch, err := cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, int64(6), epoch)
	assert.Equal(t, 1, loader.callCount())
}

func TestCachedEpochValidator_should_bypass_when_invalidation_is_down(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, time.Minute)
	_, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)

	cache.SetHealthy(false)
	_, err = cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)
	_, err = cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, 3, loader.callCount())
}

func TestCachedEpochValidator_should_reload_after_player_invalidation(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, time.Minute)
	_, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)
	loader.setEpoch(5)

	cache.Invalidate(42)
	epoch, err := cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, int64(5), epoch)
	assert.Equal(t, 2, loader.callCount())
}

func TestCachedEpochValidator_should_expire_entries(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, 30*time.Second)
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	_, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)

	now = now.Add(31 * time.Second)
	_, err = cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, 2, loader.callCount())
}

func TestCachedEpochValidator_should_disable_caching_at_zero_ttl(t *testing.T) {
	loader := &epochLoader{epoch: 4}
	cache := healthyEpochCache(loader, 0)

	_, err := cache.CurrentEpochForToken(context.Background(), 42, 4)
	require.NoError(t, err)
	_, err = cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, 2, loader.callCount())
}

func TestCachedEpochValidator_should_not_store_a_read_that_raced_invalidation(t *testing.T) {
	loader := &epochLoader{epoch: 4, start: make(chan struct{}), resume: make(chan struct{})}
	cache := healthyEpochCache(loader, time.Minute)
	done := make(chan struct{})
	go func() {
		_, _ = cache.CurrentEpochForToken(context.Background(), 42, 4)
		close(done)
	}()
	<-loader.start
	cache.Invalidate(42)
	loader.setEpoch(5)
	close(loader.resume)
	<-done

	epoch, err := cache.CurrentEpochForToken(context.Background(), 42, 4)

	require.NoError(t, err)
	assert.Equal(t, int64(5), epoch)
	assert.Equal(t, 2, loader.callCount())
}
