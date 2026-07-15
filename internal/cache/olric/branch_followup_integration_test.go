//go:build integration

package olric

import (
	"context"
	"net"
	"testing"
	"time"

	olricpkg "github.com/olric-data/olric"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
)

func freeBranchOlricPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func newBranchOlricStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := New(ctx, Config{
		BindPort:           freeBranchOlricPort(t),
		MemberlistBindPort: freeBranchOlricPort(t),
		LogLevel:           "ERROR",
		StartTimeout:       20 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		require.NoError(t, store.Close(closeCtx))
	})
	return store
}

func TestBranchFollowup_release_and_refresh_charge_persisted_burst_state(t *testing.T) {
	store := newBranchOlricStore(t)
	ctx := context.Background()
	const key = "branch:burst:charge"
	const budget = 120 * time.Millisecond
	for range 4 {
		ok, _, err := store.AcquireSlotBurst(ctx, key, 2, 4, budget, time.Hour)
		require.NoError(t, err)
		require.True(t, ok)
	}

	time.Sleep(budget + 30*time.Millisecond)
	require.NoError(t, store.ReleaseSlotBurst(ctx, key))
	state, err := store.loadBurst(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(3), state.Count)
	assert.Zero(t, state.BurstRemaining)
	require.NoError(t, store.ReleaseSlotBurst(ctx, key))
	state, err = store.loadBurst(ctx, key)
	require.NoError(t, err)
	require.Equal(t, int64(2), state.Count)
	require.Zero(t, state.BurstRemaining)
	state.LastAssessed = time.Now().Add(time.Second)
	require.NoError(t, store.burstSlots.Put(ctx, key, encodeBurst(state), olricpkg.EX(time.Hour)))

	ok, current, err := store.AcquireSlotBurst(ctx, key, 2, 4, budget, time.Hour)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(2), current)

	now := time.Now()
	seed := cache.BurstSlotState{
		Count:          2,
		BurstRemaining: 0,
		LastAssessed:   now.Add(-cache.BurstRefillWindow),
		Expires:        now.Add(time.Hour),
		Sustained:      2,
		BurstBudget:    budget,
	}
	require.NoError(t, store.burstSlots.Put(ctx, key, encodeBurst(seed), olricpkg.EX(time.Hour)))
	ok, current, err = store.AcquireSlotBurst(ctx, key, 2, 4, budget, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(3), current)
	state, err = store.loadBurst(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, budget, state.BurstRemaining)

	seed = cache.BurstSlotState{
		Count:          4,
		BurstRemaining: budget,
		LastAssessed:   time.Now().Add(-budget / 2),
		Expires:        time.Now().Add(time.Hour),
		Sustained:      2,
		BurstBudget:    budget,
	}
	require.NoError(t, store.burstSlots.Put(ctx, key, encodeBurst(seed), olricpkg.EX(time.Hour)))
	require.NoError(t, store.RefreshSlotBurst(ctx, key, time.Hour))
	state, err = store.loadBurst(ctx, key)
	require.NoError(t, err)
	assert.Less(t, state.BurstRemaining, budget)
	assert.Greater(t, state.BurstRemaining, time.Duration(0))
	assert.WithinDuration(t, time.Now().Add(time.Hour), state.Expires, 100*time.Millisecond)
}
