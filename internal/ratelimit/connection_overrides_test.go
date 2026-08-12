package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
)

type countingConnectionLimits struct {
	calls  int
	limits ratelimit.CapLimits
	found  bool
	err    error
}

func (c *countingConnectionLimits) ConnectionLimit(context.Context, int64) (ratelimit.CapLimits, bool, error) {
	c.calls++
	return c.limits, c.found, c.err
}

func TestCachedConnectionLimitStore_memoizes_and_invalidates_by_tenant(t *testing.T) {
	inner := &countingConnectionLimits{
		limits: ratelimit.CapLimits{Sustained: 250_000, Ceiling: 500_000},
		found:  true,
	}
	store := ratelimit.NewCachedConnectionLimitStore(inner, time.Hour)

	for range 2 {
		got, ok, err := store.ConnectionLimit(context.Background(), 42)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, inner.limits, got)
	}
	require.Equal(t, 1, inner.calls)

	store.Invalidate(42)
	_, _, err := store.ConnectionLimit(context.Background(), 42)
	require.NoError(t, err)
	assert.Equal(t, 2, inner.calls)
}

func TestCachedConnectionLimitStore_uses_short_error_backoff_before_retry(t *testing.T) {
	inner := &countingConnectionLimits{err: assert.AnError}
	store := ratelimit.NewCachedConnectionLimitStore(inner, 100*time.Millisecond)

	_, _, err := store.ConnectionLimit(context.Background(), 42)
	require.Error(t, err)
	inner.err = nil
	inner.found = true
	inner.limits = ratelimit.CapLimits{Sustained: 100_000, Ceiling: 200_000}
	_, ok, err := store.ConnectionLimit(context.Background(), 42)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 1, inner.calls, "the error backoff prevents hammering the database")

	time.Sleep(120 * time.Millisecond)
	got, ok, err := store.ConnectionLimit(context.Background(), 42)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, inner.limits, got)
	assert.Equal(t, 2, inner.calls)
}

func TestCachedConnectionLimitStore_serves_last_known_override_on_refresh_error(t *testing.T) {
	inner := &countingConnectionLimits{
		limits: ratelimit.CapLimits{Sustained: 250_000, Ceiling: 500_000},
		found:  true,
	}
	store := ratelimit.NewCachedConnectionLimitStore(inner, 100*time.Millisecond)

	_, _, err := store.ConnectionLimit(context.Background(), 42)
	require.NoError(t, err)
	time.Sleep(120 * time.Millisecond)
	inner.err = assert.AnError
	got, ok, err := store.ConnectionLimit(context.Background(), 42)

	require.Error(t, err)
	require.True(t, ok)
	assert.Equal(t, inner.limits, got)
	got, ok, err = store.ConnectionLimit(context.Background(), 42)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, inner.limits, got)
	assert.Equal(t, 2, inner.calls)
}

type blockingConnectionLimits struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingConnectionLimits) ConnectionLimit(context.Context, int64) (ratelimit.CapLimits, bool, error) {
	b.calls.Add(1)
	b.once.Do(func() { close(b.started) })
	<-b.release
	return ratelimit.CapLimits{Sustained: 250_000, Ceiling: 500_000}, true, nil
}

func TestCachedConnectionLimitStore_coalesces_concurrent_misses_per_tenant(t *testing.T) {
	inner := &blockingConnectionLimits{started: make(chan struct{}), release: make(chan struct{})}
	store := ratelimit.NewCachedConnectionLimitStore(inner, time.Hour)
	start := make(chan struct{})
	var wg sync.WaitGroup

	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = store.ConnectionLimit(context.Background(), 42)
		}()
	}
	close(start)
	<-inner.started
	time.Sleep(20 * time.Millisecond)
	close(inner.release)
	wg.Wait()

	assert.Equal(t, int64(1), inner.calls.Load())
}
