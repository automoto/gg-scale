package matchmaker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/realtime"
)

type fakeAllocator struct {
	mu          sync.Mutex
	called      atomic.Int64
	address     string
	err         error
	gotReqs     []fleet.AllocationRequest
	gotCtxes    []context.Context
	deallocated []fleet.AllocationID
	nextID      atomic.Int64
}

func (f *fakeAllocator) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called.Add(1)
	f.gotReqs = append(f.gotReqs, req)
	f.gotCtxes = append(f.gotCtxes, ctx)
	if f.err != nil {
		return nil, f.err
	}
	id := fleet.AllocationID(f.nextID.Add(1))
	return &fleet.Allocation{ID: id, Address: f.address, Status: fleet.StatusReady}, nil
}

// Deallocate enforces tenant context — the real fleet.Manager.Deallocate
// goes through the store, which filters by tenant; callers that forget to
// pass a tenant-tagged ctx (a real bug we shipped once) must fail here too.
func (f *fakeAllocator) Deallocate(ctx context.Context, id fleet.AllocationID) error {
	if _, err := db.TenantFromContext(ctx); err != nil {
		return fmt.Errorf("fakeAllocator.Deallocate: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deallocated = append(f.deallocated, id)
	return nil
}

// Called returns the number of Allocate invocations. Use this in tests
// that run the worker on a goroutine.
func (f *fakeAllocator) Called() int64 { return f.called.Load() }

// Deallocated returns the AllocationIDs that have been released. Tests
// assert orphan-cleanup paths by checking this.
func (f *fakeAllocator) Deallocated() []fleet.AllocationID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.AllocationID, len(f.deallocated))
	copy(out, f.deallocated)
	return out
}

type fakeNotifier struct {
	mu            sync.Mutex
	sent          []sentMessage
	failErr       error
	failForUserID int64 // when non-zero, only this player_id gets ErrNotConnected
}

type sentMessage struct {
	tenantID, playerID int64
	msg                realtime.Message
}

func (f *fakeNotifier) Send(_ context.Context, tenantID, playerID int64, msg realtime.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	if f.failForUserID != 0 && playerID == f.failForUserID {
		return realtime.ErrNotConnected
	}
	f.sent = append(f.sent, sentMessage{tenantID, playerID, msg})
	return nil
}

func (f *fakeNotifier) Sent() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func enqueue(t *testing.T, q *matchmaker.MemQueue, req matchmaker.EnqueueRequest) *matchmaker.Ticket {
	t.Helper()
	ticket, err := q.Enqueue(context.Background(), req)
	require.NoError(t, err)
	return ticket
}

func TestWorkerAllocatesAndNotifiesOnFullBucket(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	hub := &fakeNotifier{}
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(1), alloc.Called())
	sent := hub.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(1), sent[0].tenantID)
	assert.Equal(t, int64(42), sent[0].playerID)
	assert.Equal(t, "matchmaker_matched", sent[0].msg.Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(sent[0].msg.Payload, &payload))
	assert.Equal(t, "10.0.0.1:7777", payload["address"])
}

func TestWorkerLeavesTicketQueuedBelowMinCount(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, Region: "us-east-1", GameMode: "1v1", PlayerID: 1, MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(0), alloc.Called())
}

func TestWorkerForwardsTenantContextToAllocator(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 99, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	require.Len(t, alloc.gotCtxes, 1)
	tid, err := db.TenantFromContext(alloc.gotCtxes[0])
	require.NoError(t, err)
	assert.Equal(t, int64(99), tid)
	pid, ok := db.ProjectFromContext(alloc.gotCtxes[0])
	require.True(t, ok)
	assert.Equal(t, int64(7), pid)
}

func TestWorkerFailsTicketAfterMaxAttempts(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{err: errors.New("backend down")}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{MaxAttempts: 1})
	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
}

func TestWorkerRetriesUnderAttemptCap(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{err: errors.New("backend down")}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{MaxAttempts: 3})
	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status, "first allocator failure under cap should leave the ticket queued")
}

func TestWorkerDeallocatesOrphanWhenCommitFindsNoRows(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	hub := &fakeNotifier{}
	t1 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	// Race: player cancels mid-allocate. Allocate takes long enough that
	// the cancel runs between ClaimBucket and CommitClaim; CommitClaim then
	// affects 0 rows and the worker should release the orphan allocation.
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = q.Cancel(db.WithTenant(context.Background(), 1), t1.ID, 42)
	}()
	delayed := &delayingAllocator{inner: alloc, delay: 20 * time.Millisecond}
	w := matchmaker.NewWorker(q, delayed, hub, matchmaker.WorkerConfig{})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(1), alloc.Called(), "Allocate should have run once")
	require.Eventually(t, func() bool { return len(alloc.Deallocated()) == 1 }, time.Second, 5*time.Millisecond,
		"orphan allocation should have been released")
	assert.Empty(t, hub.Sent(), "no MatchReady should be sent when CommitClaim affects 0 rows")
}

// delayingAllocator wraps another allocator and sleeps in Allocate so tests
// can race a Cancel against the commit step.
type delayingAllocator struct {
	inner *fakeAllocator
	delay time.Duration
}

func (d *delayingAllocator) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	time.Sleep(d.delay)
	return d.inner.Allocate(ctx, req)
}

func (d *delayingAllocator) Deallocate(ctx context.Context, id fleet.AllocationID) error {
	return d.inner.Deallocate(ctx, id)
}

func TestWorkerIsolatesTenantsAndProjects(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 1, Region: "r", GameMode: "g", MinCount: 2, MaxCount: 2})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 2, Region: "r", GameMode: "g", MinCount: 2, MaxCount: 2})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 2, ProjectID: 7, FleetID: 5, PlayerID: 1, Region: "r", GameMode: "g", MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(1), alloc.Called())
	require.Len(t, alloc.gotReqs, 1)
	assert.Equal(t, int64(1), alloc.gotReqs[0].TenantID)
	assert.Equal(t, 2, alloc.gotReqs[0].Capacity)
}

// listenerQueue wraps a MemQueue with an in-memory Listener so the worker
// exercises the LISTEN-driven code path without needing Postgres.
type listenerQueue struct {
	*matchmaker.MemQueue
	events chan matchmaker.Bucket
}

func newListenerQueue() *listenerQueue {
	return &listenerQueue{MemQueue: matchmaker.NewMemQueue(), events: make(chan matchmaker.Bucket, 8)}
}

func (q *listenerQueue) Listen(ctx context.Context, fn func(matchmaker.Bucket)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-q.events:
			fn(b)
		}
	}
}

func TestWorkerProcessesBucketOnListenerEvent(t *testing.T) {
	q := newListenerQueue()
	alloc := &fakeAllocator{address: "10.0.0.42:7777"}
	hub := &fakeNotifier{}
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{
		Interval: time.Hour,
	})
	enqueue(t, q.MemQueue, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- matchmaker.Bucket{TenantID: 1, ProjectID: 7, Mode: matchmaker.ModeFleetAllocation, FleetID: 5, Region: "us-east-1", GameMode: "1v1"}

	require.Eventually(t, func() bool { return alloc.Called() == 1 }, 2*time.Second, 10*time.Millisecond, "worker did not wake on listener event")
	require.Eventually(t, func() bool { return len(hub.Sent()) == 1 }, 2*time.Second, 10*time.Millisecond)
}

// When the only matched player is no longer connected, the worker must
// release the allocation it just made — otherwise the fleet slot leaks and
// subsequent allocate calls fail with state=UnAllocated until the fleet is
// scaled or the server is reaped manually.
func TestWorkerReleasesAllocationWhenNoClientIsReachable(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	hub := &fakeNotifier{failErr: realtime.ErrNotConnected}
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "r", GameMode: "g"})

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, int64(1), alloc.Called())
	assert.Equal(t, []fleet.AllocationID{1}, alloc.Deallocated(),
		"allocation must be released when no client received match_ready")
}

// Multi-player match where one player is offline but others got the push:
// the allocation must NOT be released — the reachable players will still
// connect to the server and the offline one can reconnect.
func TestWorkerKeepsAllocationWhenAnyClientReachable(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	hub := &fakeNotifier{failForUserID: 42} // one of two players is offline
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 41, Region: "r", GameMode: "g", MinCount: 2, MaxCount: 2})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "r", GameMode: "g", MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, int64(1), alloc.Called())
	assert.Empty(t, alloc.Deallocated(),
		"allocation must persist when at least one client received match_ready")
}

func TestWorkerRunWaitsForGoroutinesOnShutdown(t *testing.T) {
	q := newListenerQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{
		Interval:      time.Hour,
		SweepInterval: time.Hour,
		WorkerCount:   2,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Worker.Run did not return after ctx cancel — goroutines leaked")
	}
}

func TestWorkerWithNilAllocatorFailsFleetTicketsSafely(t *testing.T) {
	q := matchmaker.NewMemQueue()
	w := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{MaxAttempts: 1})
	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status, "no allocator → ticket fails through the attempt counter, no panic")
}

func TestWorkerMatchesMatchOnlyTicketsWithSharedMatchID(t *testing.T) {
	q := matchmaker.NewMemQueue()
	hub := &fakeNotifier{}
	w := matchmaker.NewWorker(q, nil, hub, matchmaker.WorkerConfig{})

	t1 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 41, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})
	t2 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))

	tctx := db.WithTenant(context.Background(), 1)
	g1, err := q.Get(tctx, t1.ID, 41)
	require.NoError(t, err)
	g2, err := q.Get(tctx, t2.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, g1.Status)
	assert.Equal(t, matchmaker.StatusMatched, g2.Status)
	assert.NotEmpty(t, g1.MatchID)
	assert.Equal(t, g1.MatchID, g2.MatchID, "both tickets share one match id")
	assert.Empty(t, g1.MatchAddress, "match_only has no server address")

	match, err := q.GetMatch(tctx, g1.MatchID)
	require.NoError(t, err)
	require.Len(t, match.Roster, 2, "match record carries the full roster for poll recovery")
}

func TestWorkerMatchedEventIncludesAllUsers(t *testing.T) {
	q := matchmaker.NewMemQueue()
	hub := &fakeNotifier{}
	w := matchmaker.NewWorker(q, nil, hub, matchmaker.WorkerConfig{})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 41, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))

	sent := hub.Sent()
	require.Len(t, sent, 2, "one event per rostered player")
	for _, s := range sent {
		assert.Equal(t, "matchmaker_matched", s.msg.Type)
		var payload struct {
			MatchID string `json:"match_id"`
			Users   []struct {
				PlayerID int64 `json:"player_id"`
			} `json:"users"`
		}
		require.NoError(t, json.Unmarshal(s.msg.Payload, &payload))
		require.Len(t, payload.Users, 2)
	}
}

func TestWorkerCommitsMatchOnlyEvenWhenNobodyConnected(t *testing.T) {
	q := matchmaker.NewMemQueue()
	hub := &fakeNotifier{failErr: realtime.ErrNotConnected}
	w := matchmaker.NewWorker(q, nil, hub, matchmaker.WorkerConfig{})

	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeMatchOnly})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, got.Status,
		"match_only has no resource to reclaim — the match stands and is recoverable by polling")
	assert.NotEmpty(t, got.MatchID)
}

type fakeSessionCreator struct {
	mu      sync.Mutex
	calls   []fakeSessionCall
	err     error
	nextSeq int
}

type fakeSessionCall struct {
	projectID int64
	gameMode  string
	players   []int64
}

func (f *fakeSessionCreator) CreateMatchSession(_ context.Context, projectID int64, gameMode string, players []int64) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", "", f.err
	}
	f.calls = append(f.calls, fakeSessionCall{projectID, gameMode, players})
	f.nextSeq++
	return fmt.Sprintf("gs_%d", f.nextSeq), fmt.Sprintf("CODE%02d", f.nextSeq), nil
}

func TestWorkerGameSessionModeCreatesSessionForRoster(t *testing.T) {
	q := matchmaker.NewMemQueue()
	hub := &fakeNotifier{}
	sessions := &fakeSessionCreator{}
	w := matchmaker.NewWorker(q, nil, hub, matchmaker.WorkerConfig{Sessions: sessions})

	t1 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 41, Mode: matchmaker.ModeGameSession, GameMode: "coop", MinCount: 2, MaxCount: 2})
	t2 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeGameSession, GameMode: "coop", MinCount: 2, MaxCount: 2})

	require.NoError(t, w.Tick(context.Background()))

	require.Len(t, sessions.calls, 1)
	assert.Equal(t, int64(7), sessions.calls[0].projectID)
	assert.Equal(t, "coop", sessions.calls[0].gameMode)
	assert.ElementsMatch(t, []int64{41, 42}, sessions.calls[0].players)

	tctx := db.WithTenant(context.Background(), 1)
	g1, err := q.Get(tctx, t1.ID, 41)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, g1.Status)

	match, err := q.GetMatch(tctx, g1.MatchID)
	require.NoError(t, err)
	assert.Equal(t, "gs_1", match.SessionID)
	assert.Equal(t, "CODE01", match.JoinCode)

	sent := hub.Sent()
	require.Len(t, sent, 2)
	var payload struct {
		SessionID string `json:"session_id"`
		JoinCode  string `json:"join_code"`
	}
	require.NoError(t, json.Unmarshal(sent[0].msg.Payload, &payload))
	assert.Equal(t, "gs_1", payload.SessionID)
	assert.Equal(t, "CODE01", payload.JoinCode)
	_ = t2
}

func TestWorkerGameSessionModeFailsTicketsWhenSessionCreationFails(t *testing.T) {
	q := matchmaker.NewMemQueue()
	sessions := &fakeSessionCreator{err: errors.New("db down")}
	w := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{MaxAttempts: 1, Sessions: sessions})

	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeGameSession})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status,
		"session-creation failure follows the allocation-failure path")
}

func TestWorkerWidensRegionsAfterWindow(t *testing.T) {
	q := matchmaker.NewMemQueue()
	hub := &fakeNotifier{}
	w := matchmaker.NewWorker(q, nil, hub, matchmaker.WorkerConfig{RegionRelaxAfter: time.Nanosecond})

	t1 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 41, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2, AllowCrossRegion: true})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeMatchOnly, Region: "us-east-1", GameMode: "1v1", MinCount: 2, MaxCount: 2, AllowCrossRegion: true})
	time.Sleep(time.Millisecond)

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), t1.ID, 41)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusMatched, got.Status,
		"cross-region tickets group once the widen window elapses")
}

func TestWorkerNeverWidensPinnedTickets(t *testing.T) {
	q := matchmaker.NewMemQueue()
	w := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{RegionRelaxAfter: time.Nanosecond})

	t1 := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 41, Mode: matchmaker.ModeMatchOnly, Region: "eu-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, PlayerID: 42, Mode: matchmaker.ModeMatchOnly, Region: "us-east-1", GameMode: "1v1", MinCount: 2, MaxCount: 2})
	time.Sleep(time.Millisecond)

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), t1.ID, 41)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusQueued, got.Status,
		"allow_cross_region=false tickets stay pinned to their region")
}

// failCommitQueue wraps MemQueue to force CommitTickets errors, simulating a
// recurring commit failure (transient DB error or a claim/commit bug).
type failCommitQueue struct {
	*matchmaker.MemQueue
	failsLeft int
}

func (q *failCommitQueue) CommitTickets(ctx context.Context, claim *matchmaker.Claim, ids []int64, matchID, addr, proto string) (int64, error) {
	if q.failsLeft > 0 {
		q.failsLeft--
		return 0, errors.New("commit boom")
	}
	return q.MemQueue.CommitTickets(ctx, claim, ids, matchID, addr, proto)
}

// A fleet commit error must release the group through the attempt counter and
// reclaim the allocation, not un-claim the tickets penalty-free — otherwise a
// recurring commit error re-allocates and re-fails forever, churning servers.
func TestWorkerFailsFleetTicketWhenCommitErrorsPastCap(t *testing.T) {
	q := &failCommitQueue{MemQueue: matchmaker.NewMemQueue(), failsLeft: 1}
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{MaxAttempts: 1})
	ticket := enqueue(t, q.MemQueue, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, FleetID: 5, PlayerID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID, 42)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status,
		"commit error should bump attempts to the cap, not un-claim penalty-free")
	assert.Len(t, alloc.Deallocated(), 1, "the orphaned allocation should be reclaimed on commit error")
}
