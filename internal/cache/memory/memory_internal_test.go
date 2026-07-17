package memory

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreSweep_removes_expired_live_slot_counters(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	store.now = func() time.Time { return now }

	acquired, _, err := store.AcquireSlot(context.Background(), "player", 2, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, _, err = store.AcquireSlotBurst(context.Background(), "tenant", 1, 2, time.Minute, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	now = now.Add(2 * time.Second)

	store.sweep()

	_, plainExists := store.shardFor("player").slots["player"]
	_, burstExists := store.shardFor("tenant").burstSlots["tenant"]
	assert.False(t, plainExists)
	assert.False(t, burstExists)
}

func TestStoreDelete_removes_burst_slot(t *testing.T) {
	store := New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })

	acquired, _, err := store.AcquireSlotBurst(context.Background(), "tenant", 1, 2, time.Minute, time.Hour)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, store.Delete(context.Background(), "tenant"))

	_, exists := store.shardFor("tenant").burstSlots["tenant"]
	assert.False(t, exists)
}

func TestStoreConcurrentTokenBucket_never_spends_past_capacity(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	store.now = func() time.Time { return now }
	const (
		callers  = 2_000
		capacity = 137
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, err := store.TokenBucket(context.Background(), "one-bucket", capacity, 1, 1)
			if err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(capacity), allowed.Load())
}

func TestStoreConcurrentSlots_never_admit_past_limit(t *testing.T) {
	store := New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	const (
		callers = 2_000
		limit   = 113
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var allowed atomic.Int64
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, err := store.AcquireSlot(context.Background(), "one-slot", limit, time.Hour)
			if err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int64(limit), allowed.Load())
}

func TestStoreSweep_reclaims_high_cardinality_expired_entries(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	store.now = func() time.Time { return now }
	const keys = 20_000

	for i := range keys {
		key := fmt.Sprintf("key:%d", i)
		require.NoError(t, store.Set(context.Background(), key, []byte("value"), time.Second))
		acquired, _, err := store.AcquireSlot(context.Background(), key, 1, time.Second)
		require.NoError(t, err)
		require.True(t, acquired)
	}
	now = now.Add(2 * time.Second)

	store.sweep()

	remaining := 0
	for i := range store.shards {
		remaining += len(store.shards[i].kv) + len(store.shards[i].slots)
	}
	assert.Zero(t, remaining)
}
