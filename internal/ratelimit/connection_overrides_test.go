package ratelimit_test

import (
	"context"
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

func TestCachedConnectionLimitStore_does_not_cache_lookup_errors(t *testing.T) {
	inner := &countingConnectionLimits{err: assert.AnError}
	store := ratelimit.NewCachedConnectionLimitStore(inner, time.Hour)

	_, _, _ = store.ConnectionLimit(context.Background(), 42)
	inner.err = nil
	inner.found = true
	inner.limits = ratelimit.CapLimits{Sustained: 100_000, Ceiling: 200_000}
	got, ok, err := store.ConnectionLimit(context.Background(), 42)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, inner.limits, got)
	assert.Equal(t, 2, inner.calls)
}
