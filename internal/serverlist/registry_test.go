package serverlist_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/serverlist"
)

func mkHB(tenant int64, fleet, name string, players int) serverlist.Heartbeat {
	return serverlist.Heartbeat{
		AgonesName:     name,
		Fleet:          fleet,
		Address:        "10.0.0.1:7777",
		Region:         "us-east",
		Name:           name,
		CurrentPlayers: players,
		MaxPlayers:     4,
		GameMode:       "deathmatch",
		Level:          "arena_battle_starter",
		Version:        "v0.2.0",
		TenantID:       tenant,
	}
}

func TestSubmitAndList(t *testing.T) {
	r := serverlist.New(30 * time.Second)
	r.Submit(mkHB(1, "doomerang-east", "gs-1", 2))
	r.Submit(mkHB(1, "doomerang-east", "gs-2", 4))

	got := r.List(1, "doomerang-east")
	require.Len(t, got, 2)
	assert.Equal(t, "gs-1", got[0].Name)
	assert.Equal(t, 2, got[0].CurrentPlayers)
	assert.Equal(t, "gs-2", got[1].Name)
}

func TestSubmitUpsertsExistingEntry(t *testing.T) {
	r := serverlist.New(30 * time.Second)
	r.Submit(mkHB(1, "f", "gs-1", 1))
	r.Submit(mkHB(1, "f", "gs-1", 3)) // same AgonesName — should update, not double

	got := r.List(1, "f")
	require.Len(t, got, 1, "duplicate AgonesName must upsert, not append")
	assert.Equal(t, 3, got[0].CurrentPlayers)
}

func TestSubmitRejectsNewEntriesOverTenantLimit(t *testing.T) {
	r := serverlist.NewWithLimit(30*time.Second, 1)
	require.NoError(t, r.Submit(mkHB(1, "f", "gs-1", 1)))

	err := r.Submit(mkHB(1, "f", "gs-2", 1))

	assert.ErrorIs(t, err, serverlist.ErrTenantLimitExceeded)
}

func TestSubmitAllowsUpdateAtTenantLimit(t *testing.T) {
	r := serverlist.NewWithLimit(30*time.Second, 1)
	require.NoError(t, r.Submit(mkHB(1, "f", "gs-1", 1)))

	err := r.Submit(mkHB(1, "f", "gs-1", 2))

	require.NoError(t, err)
	got := r.List(1, "f")
	require.Len(t, got, 1)
	assert.Equal(t, 2, got[0].CurrentPlayers)
}

func TestListIsolatesTenants(t *testing.T) {
	r := serverlist.New(30 * time.Second)
	r.Submit(mkHB(1, "f", "gs-tenant-1", 1))
	r.Submit(mkHB(2, "f", "gs-tenant-2", 1))

	t1 := r.List(1, "f")
	t2 := r.List(2, "f")
	require.Len(t, t1, 1)
	require.Len(t, t2, 1)
	assert.Equal(t, "gs-tenant-1", t1[0].Name)
	assert.Equal(t, "gs-tenant-2", t2[0].Name)
}

func TestListIsolatesFleets(t *testing.T) {
	r := serverlist.New(30 * time.Second)
	r.Submit(mkHB(1, "doomerang-east", "gs-east", 1))
	r.Submit(mkHB(1, "doomerang-west", "gs-west", 1))

	east := r.List(1, "doomerang-east")
	require.Len(t, east, 1)
	assert.Equal(t, "gs-east", east[0].Name)
}

// Regression: with a TTL of 15s, an entry that hasn't been refreshed in
// 20s must NOT appear in List output. Without TTL filtering, dead
// servers would linger in the browser indefinitely.
func TestListFiltersStaleEntries(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	clock := &muClock{t: now}
	r := serverlist.NewWithClock(15*time.Second, clock.Now)

	r.Submit(mkHB(1, "f", "gs-fresh", 1))

	clock.Advance(20 * time.Second)
	r.Submit(mkHB(1, "f", "gs-newer", 1))

	got := r.List(1, "f")
	require.Len(t, got, 1, "stale entry must be filtered from List output")
	assert.Equal(t, "gs-newer", got[0].Name)
}

func TestSweepRemovesStaleEntries(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	clock := &muClock{t: now}
	r := serverlist.NewWithClock(15*time.Second, clock.Now)

	r.Submit(mkHB(1, "f", "gs-1", 1))
	clock.Advance(20 * time.Second)
	r.Sweep()
	got := r.List(1, "f")
	assert.Empty(t, got, "Sweep must remove entries older than TTL")
}

func TestRunGCStopsOnContextCancel(t *testing.T) {
	r := serverlist.New(15 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunGC(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunGC did not return after ctx cancel")
	}
}

// muClock is a manually-advanced clock for deterministic TTL tests.
type muClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *muClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *muClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
