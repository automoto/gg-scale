package matchmaker_test

import (
	"context"
	"encoding/json"
	"errors"
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
	mu       sync.Mutex
	called   atomic.Int64
	address  string
	err      error
	gotReqs  []fleet.AllocationRequest
	gotCtxes []context.Context
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
	return &fleet.Allocation{Address: f.address, Status: fleet.StatusReady}, nil
}

// Called returns the number of Allocate invocations. Use this in tests
// that run the worker on a goroutine.
func (f *fakeAllocator) Called() int64 { return f.called.Load() }

type fakeNotifier struct {
	mu      sync.Mutex
	sent    []sentMessage
	failErr error
}

type sentMessage struct {
	tenantID, endUserID int64
	msg                 realtime.Message
}

func (f *fakeNotifier) Send(_ context.Context, tenantID, endUserID int64, msg realtime.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.sent = append(f.sent, sentMessage{tenantID, endUserID, msg})
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
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{BucketSize: 1})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(1), alloc.Called())
	sent := hub.Sent()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(1), sent[0].tenantID)
	assert.Equal(t, int64(42), sent[0].endUserID)
	assert.Equal(t, "match_ready", sent[0].msg.Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(sent[0].msg.Payload, &payload))
	assert.Equal(t, "10.0.0.1:7777", payload["address"])
}

func TestWorkerSkipsBucketsThatDoNotMeetSize(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{BucketSize: 2})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, Region: "us-east-1", GameMode: "1v1", EndUserID: 1})

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int64(0), alloc.Called())
}

func TestWorkerForwardsTenantContextToAllocator(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{BucketSize: 1})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 99, ProjectID: 7, EndUserID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	require.Len(t, alloc.gotCtxes, 1)
	tid, err := db.TenantFromContext(alloc.gotCtxes[0])
	require.NoError(t, err)
	assert.Equal(t, int64(99), tid)
	pid, ok := db.ProjectFromContext(alloc.gotCtxes[0])
	require.True(t, ok)
	assert.Equal(t, int64(7), pid)
}

func TestWorkerMarksTicketsFailedWhenAllocateFails(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{err: errors.New("backend down")}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{BucketSize: 1})
	ticket := enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 42, Region: "us-east-1", GameMode: "1v1"})

	require.NoError(t, w.Tick(context.Background()))

	got, err := q.Get(db.WithTenant(context.Background(), 1), ticket.ID)
	require.NoError(t, err)
	assert.Equal(t, matchmaker.StatusFailed, got.Status)
}

func TestWorkerIsolatesTenantsAndProjects(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	w := matchmaker.NewWorker(q, alloc, nil, matchmaker.WorkerConfig{BucketSize: 2})

	// Two tickets for tenant 1 / project 7 -> bucket fills.
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 1, Region: "r", GameMode: "g"})
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 2, Region: "r", GameMode: "g"})
	// One ticket for tenant 2 / project 7 -> bucket does NOT fill.
	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 2, ProjectID: 7, EndUserID: 1, Region: "r", GameMode: "g"})

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
	// Long fallback so any processing has to come from the listener event.
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{
		BucketSize: 1,
		Interval:   time.Hour,
	})
	enqueue(t, q.MemQueue, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 42, Region: "us-east-1", GameMode: "1v1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	q.events <- matchmaker.Bucket{TenantID: 1, ProjectID: 7, Region: "us-east-1", GameMode: "1v1"}

	require.Eventually(t, func() bool { return alloc.Called() == 1 }, 2*time.Second, 10*time.Millisecond, "worker did not wake on listener event")
	require.Eventually(t, func() bool { return len(hub.Sent()) == 1 }, 2*time.Second, 10*time.Millisecond)
}

func TestWorkerToleratesNotifierErrors(t *testing.T) {
	q := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.0.0.1:7777"}
	hub := &fakeNotifier{failErr: realtime.ErrNotConnected}
	w := matchmaker.NewWorker(q, alloc, hub, matchmaker.WorkerConfig{BucketSize: 1})

	enqueue(t, q, matchmaker.EnqueueRequest{TenantID: 1, ProjectID: 7, EndUserID: 42, Region: "r", GameMode: "g"})

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, int64(1), alloc.Called())
}
