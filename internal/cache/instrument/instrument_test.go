package instrument_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/instrument"
	"github.com/ggscale/ggscale/internal/cache/memory"
)

func newStore(t *testing.T) (cache.Store, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	return instrument.New(memory.New(), reg), reg
}

func counterVal(t *testing.T, reg *prometheus.Registry, op, result string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "ggscale_cache_ops_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var gotOp, gotResult string
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "op":
					gotOp = lp.GetValue()
				case "result":
					gotResult = lp.GetValue()
				}
			}
			if gotOp == op && gotResult == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestInstrumentedStore_Get_miss_increments_miss_counter(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "missing")
	assert.ErrorIs(t, err, cache.ErrNotFound)
	assert.Equal(t, float64(1), counterVal(t, reg, "get", "miss"))
}

func TestInstrumentedStore_Get_hit_increments_hit_counter(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	require.NoError(t, store.Set(ctx, "k", []byte("v"), time.Hour))
	_, err := store.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, float64(1), counterVal(t, reg, "get", "hit"))
}

func TestInstrumentedStore_Set_increments_ok_counter(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	require.NoError(t, store.Set(ctx, "k", []byte("v"), time.Hour))
	assert.Equal(t, float64(1), counterVal(t, reg, "set", "ok"))
}

func TestInstrumentedStore_Delete_increments_ok_counter(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	require.NoError(t, store.Set(ctx, "k", []byte("v"), time.Hour))
	require.NoError(t, store.Delete(ctx, "k"))
	assert.Equal(t, float64(1), counterVal(t, reg, "delete", "ok"))
}

func TestInstrumentedStore_TokenBucket_allowed_increments_ok(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	allowed, _, err := store.TokenBucket(ctx, "tb", 10, 1, 1)
	require.NoError(t, err)
	require.True(t, allowed)
	assert.Equal(t, float64(1), counterVal(t, reg, "token_bucket", "ok"))
}

func TestInstrumentedStore_TokenBucket_denied_increments_denied(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	// Drain the bucket.
	for i := 0; i < 3; i++ {
		_, _, err := store.TokenBucket(ctx, "tb:deny", 3, 0.001, 1)
		require.NoError(t, err)
	}
	allowed, _, err := store.TokenBucket(ctx, "tb:deny", 3, 0.001, 1)
	require.NoError(t, err)
	require.False(t, allowed)
	assert.Equal(t, float64(1), counterVal(t, reg, "token_bucket", "denied"))
}

func TestInstrumentedStore_AcquireSlot_acquired_increments_ok(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	ok, _, err := store.AcquireSlot(ctx, "slot", 5, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, float64(1), counterVal(t, reg, "acquire_slot", "ok"))
}

func TestInstrumentedStore_AcquireSlot_rejected_increments_rejected(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, _, err := store.AcquireSlot(ctx, "slot:cap", 2, time.Hour)
		require.NoError(t, err)
	}
	ok, _, err := store.AcquireSlot(ctx, "slot:cap", 2, time.Hour)
	require.NoError(t, err)
	require.False(t, ok)
	assert.Equal(t, float64(1), counterVal(t, reg, "acquire_slot", "rejected"))
}

func TestInstrumentedStore_ReleaseSlot_increments_ok(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	_, _, err := store.AcquireSlot(ctx, "slot:rel", 5, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.ReleaseSlot(ctx, "slot:rel"))
	assert.Equal(t, float64(1), counterVal(t, reg, "release_slot", "ok"))
}

func TestInstrumentedStore_RefreshSlot_increments_ok(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	_, _, err := store.AcquireSlot(ctx, "slot:ref", 5, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.RefreshSlot(ctx, "slot:ref", time.Hour))
	assert.Equal(t, float64(1), counterVal(t, reg, "refresh_slot", "ok"))
}

func TestInstrumentedStore_metric_name_is_registered(t *testing.T) {
	store, reg := newStore(t)
	ctx := context.Background()

	// Any op causes the counter family to appear in output.
	_ = store.Delete(ctx, "nonexistent")
	count := testutil.CollectAndCount(reg)
	assert.Equal(t, 1, count)
}
