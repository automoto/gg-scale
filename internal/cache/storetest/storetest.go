// Package storetest holds the cache.Store contract test suite. Each backend
// (memory, olric) imports it and runs RunSuite against a fresh Store.
package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

	t.Run("AcquireSlotBurst_admits_within_ceiling_and_walls_at_it", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		// Full budget at a single instant: connections up to the ceiling admit.
		for i := 0; i < 4; i++ {
			ok, _, err := store.AcquireSlotBurst(ctx, "burst:cap", 2, 4, 10*time.Minute, time.Hour)
			require.NoError(t, err)
			require.True(t, ok, "connection %d within burst envelope", i+1)
		}
		// Ceiling is a hard wall; current holds at the ceiling.
		ok, cur, err := store.AcquireSlotBurst(ctx, "burst:cap", 2, 4, 10*time.Minute, time.Hour)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, int64(4), cur, "rejection reports the unchanged count at the ceiling")
	})

	t.Run("AcquireSlotBurst_is_atomic_under_contention", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()
		const (
			callers = 64
			ceiling = 8
		)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var allowed atomic.Int64
		errs := make(chan error, callers)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, _, err := store.AcquireSlotBurst(ctx, "burst:concurrent", ceiling/2, ceiling, 10*time.Minute, time.Hour)
				if err != nil {
					errs <- err
					return
				}
				if ok {
					allowed.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		assert.Equal(t, int64(ceiling), allowed.Load())
	})

	t.Run("ReleaseSlotBurst_frees_a_burst_slot", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		for i := 0; i < 4; i++ {
			_, _, err := store.AcquireSlotBurst(ctx, "burst:rel", 2, 4, 10*time.Minute, time.Hour)
			require.NoError(t, err)
		}
		require.NoError(t, store.ReleaseSlotBurst(ctx, "burst:rel"))
		ok, cur, err := store.AcquireSlotBurst(ctx, "burst:rel", 2, 4, 10*time.Minute, time.Hour)
		require.NoError(t, err)
		assert.True(t, ok, "a released burst slot frees room under the ceiling")
		assert.Equal(t, int64(4), cur)
	})

	t.Run("plain_and_burst_slots_are_independent_under_one_key", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		// Two plain slots and one burst slot, all under the SAME key.
		_, _, err := store.AcquireSlot(ctx, "shared:key", 5, time.Hour)
		require.NoError(t, err)
		_, _, err = store.AcquireSlot(ctx, "shared:key", 5, time.Hour)
		require.NoError(t, err)
		_, _, err = store.AcquireSlotBurst(ctx, "shared:key", 2, 4, 10*time.Minute, time.Hour)
		require.NoError(t, err)

		// Releasing the burst counter (even spuriously) must leave the plain
		// counter at 2 — the next plain acquire reads 3.
		require.NoError(t, store.ReleaseSlotBurst(ctx, "shared:key"))
		require.NoError(t, store.ReleaseSlotBurst(ctx, "shared:key"))
		_, pcur, err := store.AcquireSlot(ctx, "shared:key", 5, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(3), pcur, "plain counter unaffected by ReleaseSlotBurst")

		// And draining the plain counter must leave the burst counter (now 0
		// after the two releases) untouched — the next burst acquire reads 1.
		for i := 0; i < 5; i++ {
			require.NoError(t, store.ReleaseSlot(ctx, "shared:key"))
		}
		_, bcur, err := store.AcquireSlotBurst(ctx, "shared:key", 2, 4, 10*time.Minute, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(1), bcur, "burst counter unaffected by ReleaseSlot")
	})

	t.Run("RefreshSlotBurst_does_not_resurrect_an_expired_slot", func(t *testing.T) {
		store := f(t)
		ctx := context.Background()

		// A burst slot that lapses its short TTL.
		_, _, err := store.AcquireSlotBurst(ctx, "burst:expire", 2, 4, 10*time.Minute, 40*time.Millisecond)
		require.NoError(t, err)
		time.Sleep(120 * time.Millisecond)

		// Refreshing the lapsed slot must not revive it.
		require.NoError(t, store.RefreshSlotBurst(ctx, "burst:expire", time.Hour))

		// The next acquire sees a fresh counter (count resets to 1), proving the
		// stale state was reclaimed rather than kept alive by the refresh.
		_, cur, err := store.AcquireSlotBurst(ctx, "burst:expire", 2, 4, 10*time.Minute, time.Hour)
		require.NoError(t, err)
		assert.Equal(t, int64(1), cur, "expired slot reclaimed; refresh did not resurrect it")
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
