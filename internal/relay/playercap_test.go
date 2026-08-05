package relay

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveCells counts cells holding any state, so a test can assert that forged
// traffic does not enlarge the limiter's footprint.
func liveCells(lim *playerAllocLimiter) int {
	n := 0
	for i := range lim.cells {
		if !lim.cells[i].last.IsZero() {
			n++
		}
	}
	return n
}

func TestPlayerAllocLimiterThrottlesFloodPerPlayer(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(6, 3) // burst 3
	lim.now = func() time.Time { return now }

	// The burst is consumed, then the flood is throttled...
	assert.True(t, lim.allow(1, 100))
	assert.True(t, lim.allow(1, 100))
	assert.True(t, lim.allow(1, 100))
	assert.False(t, lim.allow(1, 100), "4th op in the same instant is throttled")
	assert.Equal(t, int64(1), lim.Throttled())

	// ...but a different player is unaffected (per-player, not global).
	assert.True(t, lim.allow(1, 200))

	// The same player-id under a different tenant is a distinct subject.
	assert.True(t, lim.allow(2, 100))

	// Tokens refill over time: 6/min = 1 per 10s.
	now = now.Add(10 * time.Second)
	assert.True(t, lim.allow(1, 100), "a token refills after 10s")
	assert.False(t, lim.allow(1, 100))
}

func TestPlayerAllocLimiterRefillIsCappedAtBurst(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(60, 5) // 1/s, burst 5
	lim.now = func() time.Time { return now }

	for range 5 {
		assert.True(t, lim.allow(1, 42))
	}
	assert.False(t, lim.allow(1, 42), "burst exhausted")

	// Idle far longer than a full refill takes: the ceiling is still the burst.
	now = now.Add(time.Hour)
	for range 5 {
		assert.True(t, lim.allow(1, 42))
	}
	assert.False(t, lim.allow(1, 42), "refill is capped at the burst")
}

func TestPlayerAllocLimiterDistinctAuthenticatedSubjectsDoNotGrowState(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(60000, 1000)
	lim.now = func() time.Time { return now }

	before := len(lim.cells)
	for i := range 1_000 {
		lim.allow(1, int64(10_000+i))
	}

	assert.Len(t, lim.cells, before, "the cell array is fixed and never reallocates")
	assert.LessOrEqual(t, liveCells(lim), 1_000, "a subject touches at most its own cell")
}

func TestPlayerAllocLimiterSustainedDistinctSubjectsCannotGrowStateOrLockOutRealClient(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(60000, 1000)
	lim.now = func() time.Time { return now }

	subjects := make([]int64, 0, 12_000)
	next := int64(1_000_000)
	before := len(lim.cells)

	for range 200 {
		for range 50 {
			lim.allow(1, next)
			lim.allow(1, next)
			subjects = append(subjects, next)
			next++
		}
		for _, id := range subjects {
			lim.allow(1, id)
		}
		now = now.Add(30 * time.Second)
	}

	assert.Len(t, lim.cells, before, "sustained traffic does not grow the limiter")

	// The harm that matters: a real client arriving afterwards is served
	// normally, however long the flood has been running.
	assert.True(t, lim.allow(2, 7), "a real client's first op is allowed")
	assert.True(t, lim.allow(2, 7), "a real client's repeat op is allowed")
	assert.True(t, lim.allow(2, 7), "the real client is not locked out")
}

// A relay restart re-authenticates every live player at once. With no admission
// control there is nothing for that stampede to exhaust.
//
// The burst here is set well above the busiest cell this many players can
// produce. Subjects share cells, so with a burst near the expected occupancy a
// handful of colliding players would be throttled — real behaviour, and the
// accepted cost of fixed state, but not what this test is about.
func TestPlayerAllocLimiterAdmitsManyDistinctPlayersAtOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(60, 20)
	lim.now = func() time.Time { return now }

	for i := range 20_000 {
		assert.True(t, lim.allow(1, int64(500_000+i)), "player %d admitted", i)
	}
	assert.Zero(t, lim.Throttled(), "a restart stampede is not throttled")
}

// Sharing is the trade fixed state buys, so state it outright: two subjects that
// land in the same cell draw on one budget. The budget sits far above a real
// client's operation rate, so this costs a colliding player nothing in practice.
func TestPlayerAllocLimiterSubjectsSharingACellShareABudget(t *testing.T) {
	now := time.Unix(1000, 0)
	lim := newPlayerAllocLimiter(60, 2) // burst 2
	lim.now = func() time.Time { return now }

	// Pigeonhole principle: allocCells+1 subjects guarantee a collision.
	seen := make(map[uint64]int64, allocCells)
	var first, second int64
	for i := int64(1); i <= allocCells+1; i++ {
		idx := lim.cellIndex(1, i)
		if previous, ok := seen[idx]; ok {
			first, second = previous, i
			break
		}
		seen[idx] = i
	}
	require.NotZero(t, first)
	require.NotZero(t, second)

	assert.True(t, lim.allow(1, first))
	assert.True(t, lim.allow(1, second))
	assert.False(t, lim.allow(1, first), "the shared burst is spent by both subjects together")
}

func TestPlayerAllocLimiterCellIndexIsStableAndIndependentlySeeded(t *testing.T) {
	a := newPlayerAllocLimiter(60, 5)
	b := newPlayerAllocLimiter(60, 5)

	assert.Equal(t, a.cellIndex(1, 42), a.cellIndex(1, 42), "same subject, same cell")

	// Distinct tenants and limiter instances may collide for one input by
	// design. Compare a set so the test does not make a probabilistic assertion
	// about a specific pair.
	tenantDiffers := false
	seedDiffers := false
	for i := range 64 {
		if a.cellIndex(1, int64(i)) != a.cellIndex(2, int64(i)) {
			tenantDiffers = true
		}
		if a.cellIndex(1, int64(i)) != b.cellIndex(1, int64(i)) {
			seedDiffers = true
		}
	}
	assert.True(t, tenantDiffers, "tenant is part of the subject")
	assert.True(t, seedDiffers, "each limiter uses an independent seed")
}

func TestPlayerAllocLimiterDisabled(t *testing.T) {
	assert.Nil(t, newPlayerAllocLimiter(0, 20), "zero rate disables")
	assert.Nil(t, newPlayerAllocLimiter(6, 0), "zero burst disables")

	var lim *playerAllocLimiter
	for range 100 {
		assert.True(t, lim.allow(1, 100), "a nil limiter never throttles")
	}
	assert.Zero(t, lim.Throttled())
}

// The reported probe, at the layer it attacked. Forged usernames reach the
// credential callback because pion needs its key before checking integrity,
// but limiter state is now touched only at the later allocation boundary.
func TestAuthHandlerForgedUsernamesDoNotGrowLimiterState(t *testing.T) {
	iss := NewIssuer("shared-secret", "ggscale", time.Hour)
	s := &Server{playerLimiter: newPlayerAllocLimiter(60, 10)}
	auth := s.authHandler(iss)
	before := len(s.playerLimiter.cells)

	// Read the key id out of one legitimately issued credential, exactly as a
	// player holding a valid credential can.
	cred, err := iss.Issue(1, 1)
	require.NoError(t, err)
	parts := strings.Split(cred.Username, ":")
	require.Len(t, parts, 4)
	expires, kid := parts[0], parts[3]

	for i := range 1_000 {
		username := fmt.Sprintf("%s:%d:%d:%s", expires, 7, 500_000+i, kid)
		key, ok := auth(username, "ggscale", nil)
		require.True(t, ok, "a well-formed username still resolves a key for pion to verify")
		require.NotEmpty(t, key)
	}

	assert.Len(t, s.playerLimiter.cells, before,
		"1000 forged pre-authentication identities must not grow the limiter")
	assert.Zero(t, liveCells(s.playerLimiter), "forged identities touch no limiter cells")
}
