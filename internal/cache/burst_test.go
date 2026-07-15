package cache_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
)

// admit is a small helper: run AdmitBurst n times at a fixed instant and report
// how many were admitted, leaving st advanced.
func admit(st *cache.BurstSlotState, sustained, ceiling int64, budget, ttl time.Duration, now time.Time, n int) int {
	got := 0
	for i := 0; i < n; i++ {
		if cache.AdmitBurst(st, sustained, ceiling, budget, ttl, now) {
			got++
		}
	}
	return got
}

func TestAdmitBurst_allows_up_to_sustained_without_touching_budget(t *testing.T) {
	st := &cache.BurstSlotState{}
	t0 := time.Unix(0, 0)

	got := admit(st, 100, 200, 10*time.Minute, time.Hour, t0, 100)

	assert.Equal(t, 100, got)
	assert.Equal(t, int64(100), st.Count)
	assert.Equal(t, 10*time.Minute, st.BurstRemaining, "at/below sustained never drains budget")
}

func TestAdmitBurst_spike_to_2x_is_allowed_within_budget(t *testing.T) {
	st := &cache.BurstSlotState{}
	t0 := time.Unix(0, 0)

	// A single instant reconnect storm to exactly ceiling: all admitted.
	got := admit(st, 100, 200, 10*time.Minute, time.Hour, t0, 200)

	assert.Equal(t, 200, got)
	assert.Equal(t, int64(200), st.Count)
}

func TestAdmitBurst_ceiling_is_a_hard_wall(t *testing.T) {
	st := &cache.BurstSlotState{}
	t0 := time.Unix(0, 0)

	got := admit(st, 100, 200, 10*time.Minute, time.Hour, t0, 250)

	assert.Equal(t, 200, got, "never past ceiling even with full budget")
	assert.Equal(t, int64(200), st.Count)
}

func TestAdmitBurst_budget_exhaustion_clamps_to_sustained(t *testing.T) {
	st := &cache.BurstSlotState{}
	t0 := time.Unix(0, 0)
	// Fill to 2× (ceiling) at t0.
	admit(st, 100, 200, 10*time.Minute, time.Hour, t0, 200)

	// Hold 2× for the full 10-minute budget, then attempt one more connection.
	// (Count is already at ceiling so this attempt drains then rejects at the
	// ceiling; drop one first to test the budget wall specifically.)
	st.Count = 150 // sitting mid-burst
	later := t0.Add(20 * time.Minute)
	admitted := cache.AdmitBurst(st, 100, 200, 10*time.Minute, time.Hour, later)

	assert.False(t, admitted, "budget exhausted: no admission above sustained")
	assert.Equal(t, time.Duration(0), st.BurstRemaining)

	// But a connection that keeps us at/below sustained still works.
	st.Count = 99
	assert.True(t, cache.AdmitBurst(st, 100, 200, 10*time.Minute, time.Hour, later),
		"sustained capacity is always available")
}

func TestAdmitBurst_refills_over_the_window_while_at_or_below_sustained(t *testing.T) {
	st := &cache.BurstSlotState{}
	t0 := time.Unix(0, 0)
	admit(st, 100, 200, 10*time.Minute, time.Hour, t0, 150)

	// Drain the budget by camping above sustained for 10 minutes...
	st.Count = 200
	drained := t0.Add(10 * time.Minute)
	cache.AdmitBurst(st, 100, 200, 10*time.Minute, time.Hour, drained) // rejected at ceiling, but assesses
	// Force an assess below sustained to observe refill: sit at sustained for
	// a full window.
	st.Count = 50
	refilled := drained.Add(time.Hour)
	cache.AdmitBurst(st, 100, 200, 10*time.Minute, time.Hour, refilled)

	assert.Equal(t, 10*time.Minute, st.BurstRemaining, "a full window at/below sustained refills the budget")
}

func TestAdmitBurst_drains_proportionally_to_how_far_above_sustained(t *testing.T) {
	// At exactly 2× sustained the budget drains 1:1 with wall time.
	st := &cache.BurstSlotState{Count: 200, BurstRemaining: 10 * time.Minute, LastAssessed: time.Unix(0, 0)}
	t0 := time.Unix(0, 0)

	// 4 minutes at 2× → drains 4 minutes of budget (ratio (200-100)/100 = 1).
	cache.AdmitBurst(st, 100, 200, 10*time.Minute, time.Hour, t0.Add(4*time.Minute))

	assert.Equal(t, 6*time.Minute, st.BurstRemaining)
}

func TestReleaseBurst_charges_elapsed_time_before_count_drops(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	st := cache.BurstSlotState{}
	require.Equal(t, 4, admit(&st, 2, 4, 10*time.Minute, time.Hour, t0, 4))

	cache.ReleaseBurst(&st, t0.Add(10*time.Minute))
	cache.ReleaseBurst(&st, t0.Add(10*time.Minute))
	cache.ReleaseBurst(&st, t0.Add(10*time.Minute))
	cache.ReleaseBurst(&st, t0.Add(10*time.Minute))

	assert.Equal(t, time.Duration(0), st.BurstRemaining)
	assert.Equal(t, 2, admit(&st, 2, 4, 10*time.Minute, time.Hour, t0.Add(10*time.Minute), 4))
}

func TestRefreshBurst_charges_elapsed_time_at_the_live_count(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	st := cache.BurstSlotState{}
	require.Equal(t, 4, admit(&st, 2, 4, 10*time.Minute, time.Hour, t0, 4))

	cache.RefreshBurst(&st, time.Hour, t0.Add(5*time.Minute))

	assert.Equal(t, 5*time.Minute, st.BurstRemaining)
	assert.Equal(t, t0.Add(65*time.Minute), st.Expires)
}
