// Package storetest holds the cache.Store contract test suite. Each backend
// (memory, olric) imports it and runs RunSuite against a fresh Store.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
)

// Factory builds a fresh Store for one test. The returned cleanup runs at
// test end (testing.T.Cleanup-style); the test owns calling it.
type Factory func(t *testing.T) cache.Store

// RunSuite exercises every Store method against the backend produced by f.
// Subtests run serially: factories that wrap a clustered backend (Olric)
// hit a global init race in the upstream library when multiple instances
// boot in parallel, and the contract tests are quick enough that serial
// execution is not a meaningful cost.
func RunSuite(t *testing.T, f Factory) {
	t.Helper()

	t.Run("TokenBucket_allows_until_capacity_then_rejects", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 5; i++ {
			ok, _, err := store.TokenBucket(ctx, "tb:fill", 5, 1, 1)
			require.NoError(t, err)
			require.True(t, ok, "fill request %d should pass", i)
		}

		ok, retry, err := store.TokenBucket(ctx, "tb:fill", 5, 1, 1)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Greater(t, retry, time.Duration(0))
	})

	t.Run("TokenBucket_refills_over_time", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 10; i++ {
			_, _, err := store.TokenBucket(ctx, "tb:refill", 10, 100, 1)
			require.NoError(t, err)
		}
		ok, _, err := store.TokenBucket(ctx, "tb:refill", 10, 100, 1)
		require.NoError(t, err)
		require.False(t, ok, "bucket must be empty after draining 10 of 10")

		time.Sleep(200 * time.Millisecond)

		ok, _, err = store.TokenBucket(ctx, "tb:refill", 10, 100, 1)
		require.NoError(t, err)
		assert.True(t, ok, "200ms at 100 tokens/s should refill enough for one")
	})

	t.Run("TokenBucket_isolates_keys", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 5; i++ {
			_, _, err := store.TokenBucket(ctx, "tb:keyA", 5, 1, 1)
			require.NoError(t, err)
		}
		ok, _, err := store.TokenBucket(ctx, "tb:keyA", 5, 1, 1)
		require.NoError(t, err)
		require.False(t, ok)

		ok, _, err = store.TokenBucket(ctx, "tb:keyB", 5, 1, 1)
		require.NoError(t, err)
		assert.True(t, ok, "key B is independent of A")
	})

	t.Run("AcquireSlot_succeeds_within_limit", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		ok, cur, err := store.AcquireSlot(ctx, "slot:within", 5, time.Hour)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, int64(1), cur)
	})

	t.Run("AcquireSlot_at_limit_rejects_and_rolls_back", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 5; i++ {
			ok, _, err := store.AcquireSlot(ctx, "slot:cap", 5, time.Hour)
			require.NoError(t, err)
			require.True(t, ok)
		}

		ok, cur, err := store.AcquireSlot(ctx, "slot:cap", 5, time.Hour)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, int64(5), cur, "must roll back to limit, not 6")
	})

	t.Run("ReleaseSlot_frees_a_slot_and_clamps_at_zero", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			_, _, err := store.AcquireSlot(ctx, "slot:rel", 3, time.Hour)
			require.NoError(t, err)
		}
		require.NoError(t, store.ReleaseSlot(ctx, "slot:rel"))
		require.NoError(t, store.ReleaseSlot(ctx, "slot:rel"))
		require.NoError(t, store.ReleaseSlot(ctx, "slot:rel"))
		// Spurious extra Release: must not go negative.
		require.NoError(t, store.ReleaseSlot(ctx, "slot:rel"))

		ok, cur, err := store.AcquireSlot(ctx, "slot:rel", 3, time.Hour)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, int64(1), cur, "counter clamps at 0; next acquire reads 1, not 0")
	})

	t.Run("RefreshSlot_extends_idle_TTL", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		_, _, err := store.AcquireSlot(ctx, "slot:refresh", 5, 100*time.Millisecond)
		require.NoError(t, err)

		// Refresh repeatedly while sleeping past the original TTL.
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			require.NoError(t, store.RefreshSlot(ctx, "slot:refresh", 100*time.Millisecond))
			time.Sleep(40 * time.Millisecond)
		}

		ok, cur, err := store.AcquireSlot(ctx, "slot:refresh", 5, 100*time.Millisecond)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, int64(2), cur, "Refresh kept the counter alive; next acquire is the second slot")
	})

	t.Run("Set_then_Get_round_trips_value_within_TTL", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		require.NoError(t, store.Set(ctx, "kv:roundtrip", []byte("hello"), time.Hour))
		got, err := store.Get(ctx, "kv:roundtrip")
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), got)
	})

	t.Run("Get_returns_ErrNotFound_when_absent", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		_, err := store.Get(ctx, "kv:missing")
		assert.True(t, errors.Is(err, cache.ErrNotFound), "wanted ErrNotFound, got %v", err)
	})

	t.Run("Get_returns_ErrNotFound_after_TTL_expires", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		require.NoError(t, store.Set(ctx, "kv:ttl", []byte("brief"), 50*time.Millisecond))
		time.Sleep(150 * time.Millisecond)

		_, err := store.Get(ctx, "kv:ttl")
		assert.True(t, errors.Is(err, cache.ErrNotFound))
	})

	t.Run("Delete_removes_key_and_is_idempotent", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		require.NoError(t, store.Set(ctx, "kv:del", []byte("x"), time.Hour))
		require.NoError(t, store.Delete(ctx, "kv:del"))

		_, err := store.Get(ctx, "kv:del")
		assert.True(t, errors.Is(err, cache.ErrNotFound))

		// Second delete must not error.
		require.NoError(t, store.Delete(ctx, "kv:del"))
	})
}
