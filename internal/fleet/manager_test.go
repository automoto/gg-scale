package fleet_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
)

// fakeBackend lets each test script backend behaviour without standing up a
// Docker daemon. allocateImpl is invoked on each Allocate call so tests can
// assert retry semantics; watchImpl supplies the StatusUpdate stream.
type fakeBackend struct {
	mu             sync.Mutex
	name           string
	allocateCalls  int
	deallocateIDs  []fleet.AllocationID
	allocateImpl   func(int) (*fleet.Allocation, error)
	watchImpl      func() <-chan fleet.StatusUpdate
	deallocateImpl func(fleet.AllocationID, string) error
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	f.mu.Lock()
	f.allocateCalls++
	n := f.allocateCalls
	f.mu.Unlock()
	return f.allocateImpl(n)
}

func (f *fakeBackend) Deallocate(_ context.Context, id fleet.AllocationID, ref string) error {
	f.mu.Lock()
	f.deallocateIDs = append(f.deallocateIDs, id)
	f.mu.Unlock()
	if f.deallocateImpl != nil {
		return f.deallocateImpl(id, ref)
	}
	return nil
}

func (f *fakeBackend) Status(_ context.Context, _ fleet.AllocationID, _ string) (fleet.Status, error) {
	return fleet.StatusReady, nil
}

func (f *fakeBackend) Watch(_ context.Context, _ fleet.AllocationID, _ string) (<-chan fleet.StatusUpdate, error) {
	if f.watchImpl == nil {
		ch := make(chan fleet.StatusUpdate)
		close(ch)
		return ch, nil
	}
	return f.watchImpl(), nil
}

func (f *fakeBackend) HealthCheck(_ context.Context) error { return nil }

// fakeStore is an in-memory replacement for the Postgres-backed store. It
// hands out monotonic IDs and lets tests inspect the persisted lifecycle.
type fakeStore struct {
	mu          sync.Mutex
	next        fleet.AllocationID
	allocations map[fleet.AllocationID]*fleet.Allocation
}

func newFakeStore() *fakeStore {
	return &fakeStore{allocations: map[fleet.AllocationID]*fleet.Allocation{}}
}

func (s *fakeStore) InsertPending(_ context.Context, req fleet.AllocationRequest, backend string) (fleet.AllocationID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	a := &fleet.Allocation{
		ID:        s.next,
		TenantID:  req.TenantID,
		ProjectID: req.ProjectID,
		Backend:   backend,
		Region:    req.Region,
		Status:    fleet.StatusPending,
		Metadata:  req.Labels,
	}
	s.allocations[s.next] = a
	return s.next, nil
}

func (s *fakeStore) MarkReady(_ context.Context, id fleet.AllocationID, ref, address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocations[id]
	if !ok {
		return fleet.ErrNotFound
	}
	a.BackendRef = ref
	a.Address = address
	a.Status = fleet.StatusReady
	return nil
}

func (s *fakeStore) MarkFailed(_ context.Context, id fleet.AllocationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocations[id]
	if !ok {
		return fleet.ErrNotFound
	}
	a.Status = fleet.StatusFailed
	return nil
}

func (s *fakeStore) Release(_ context.Context, id fleet.AllocationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocations[id]
	if !ok {
		return fleet.ErrNotFound
	}
	a.Status = fleet.StatusShutdown
	return nil
}

func (s *fakeStore) Get(_ context.Context, id fleet.AllocationID) (*fleet.Allocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.allocations[id]
	if !ok {
		return nil, fleet.ErrNotFound
	}
	clone := *a
	return &clone, nil
}

func sampleReq() fleet.AllocationRequest {
	return fleet.AllocationRequest{
		TenantID:  1,
		ProjectID: 2,
		Region:    "us-east-1",
		GameMode:  "deathmatch",
		Capacity:  16,
	}
}

func TestManager_Allocate_persists_pending_then_ready(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "container-abc", Address: "10.0.0.1:7777"}, nil
		},
	}
	mgr := fleet.NewManager(store, backend, fleet.ManagerOptions{Retries: 0, Clock: zeroClock})

	got, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Equal(t, fleet.StatusReady, got.Status)
	assert.Equal(t, "container-abc", got.BackendRef)
	assert.Equal(t, "10.0.0.1:7777", got.Address)

	persisted, err := store.Get(context.Background(), got.ID)
	require.NoError(t, err)
	assert.Equal(t, fleet.StatusReady, persisted.Status)
}

func TestManager_Allocate_retries_then_succeeds(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(attempt int) (*fleet.Allocation, error) {
			if attempt < 3 {
				return nil, errors.New("transient")
			}
			return &fleet.Allocation{BackendRef: "ok", Address: "10.0.0.2:7777"}, nil
		},
	}
	mgr := fleet.NewManager(store, backend, fleet.ManagerOptions{Retries: 3, Clock: zeroClock})

	got, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Equal(t, 3, backend.allocateCalls)
	assert.Equal(t, fleet.StatusReady, got.Status)
}

func TestManager_Allocate_marks_failed_after_exhausting_retries(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return nil, errors.New("backend down")
		},
	}
	mgr := fleet.NewManager(store, backend, fleet.ManagerOptions{Retries: 2, Clock: zeroClock})

	got, err := mgr.Allocate(context.Background(), sampleReq())
	require.Error(t, err)
	require.Nil(t, got)
	assert.Equal(t, 3, backend.allocateCalls, "1 initial attempt + 2 retries")

	// Find the persisted row (we didn't get its ID back since Allocate
	// failed) — the fakeStore assigned id=1.
	persisted, err := store.Get(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, fleet.StatusFailed, persisted.Status)
}

func TestManager_Deallocate_calls_backend_and_releases_row(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "ref-1", Address: "10.0.0.1:7777"}, nil
		},
	}
	mgr := fleet.NewManager(store, backend, fleet.ManagerOptions{Clock: zeroClock})
	a, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)

	require.NoError(t, mgr.Deallocate(context.Background(), a.ID))
	assert.Equal(t, []fleet.AllocationID{a.ID}, backend.deallocateIDs)

	persisted, err := store.Get(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, fleet.StatusShutdown, persisted.Status)
}

func TestManager_Watch_pipes_backend_updates_to_caller(t *testing.T) {
	store := newFakeStore()
	src := make(chan fleet.StatusUpdate, 3)
	src <- fleet.StatusUpdate{Status: fleet.StatusAllocating}
	src <- fleet.StatusUpdate{Status: fleet.StatusReady, Address: "10.0.0.3:7777"}
	close(src)

	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "ref-2", Address: "10.0.0.3:7777"}, nil
		},
		watchImpl: func() <-chan fleet.StatusUpdate { return src },
	}
	mgr := fleet.NewManager(store, backend, fleet.ManagerOptions{Clock: zeroClock})
	a, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)

	out, err := mgr.Watch(context.Background(), a.ID)
	require.NoError(t, err)

	got := drainStatuses(t, out)
	assert.Equal(t, []fleet.Status{fleet.StatusAllocating, fleet.StatusReady}, got)
}

func zeroClock(_ int) time.Duration { return 0 }

func drainStatuses(t *testing.T, ch <-chan fleet.StatusUpdate) []fleet.Status {
	t.Helper()
	var out []fleet.Status
	timeout := time.After(2 * time.Second)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u.Status)
		case <-timeout:
			t.Fatal("timed out draining status channel")
			return out
		}
	}
}
