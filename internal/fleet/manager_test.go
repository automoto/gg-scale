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
	deallocateRefs []string
	allocateImpl   func(int) (*fleet.Allocation, error)
	watchImpl      func() <-chan fleet.StatusUpdate
	deallocateImpl func(context.Context, fleet.AllocationID, string) error
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	f.mu.Lock()
	f.allocateCalls++
	n := f.allocateCalls
	f.mu.Unlock()
	return f.allocateImpl(n)
}

func (f *fakeBackend) Deallocate(ctx context.Context, id fleet.AllocationID, ref string) error {
	f.mu.Lock()
	f.deallocateIDs = append(f.deallocateIDs, id)
	f.deallocateRefs = append(f.deallocateRefs, ref)
	f.mu.Unlock()
	if f.deallocateImpl != nil {
		return f.deallocateImpl(ctx, id, ref)
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
	mu                 sync.Mutex
	next               fleet.AllocationID
	allocations        map[fleet.AllocationID]*fleet.Allocation
	events             []fleet.Event
	markReadyErr       error
	markReadyCancel    context.CancelFunc
	markFailedIDs      []fleet.AllocationID
	markFailedActive   bool
	markFailedDeadline bool
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
	if s.markReadyCancel != nil {
		s.markReadyCancel()
	}
	if s.markReadyErr != nil {
		return s.markReadyErr
	}
	a, ok := s.allocations[id]
	if !ok {
		return fleet.ErrNotFound
	}
	a.BackendRef = ref
	a.Address = address
	a.Status = fleet.StatusReady
	return nil
}

func (s *fakeStore) MarkFailed(ctx context.Context, id fleet.AllocationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markFailedIDs = append(s.markFailedIDs, id)
	s.markFailedActive = ctx.Err() == nil
	_, s.markFailedDeadline = ctx.Deadline()
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

func (s *fakeStore) List(_ context.Context, projectID int64, includeTerminal bool, limit, offset int) ([]*fleet.Allocation, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []*fleet.Allocation
	for _, a := range s.allocations {
		if a.ProjectID != projectID {
			continue
		}
		if !includeTerminal && a.Status.IsTerminal() {
			continue
		}
		clone := *a
		matched = append(matched, &clone)
	}
	total := int64(len(matched))
	if offset >= len(matched) {
		return nil, total, nil
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total, nil
}

func (s *fakeStore) AppendEvent(_ context.Context, id fleet.AllocationID, status fleet.Status, address, errMessage string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fleet.Event{
		ID:           int64(len(s.events) + 1),
		AllocationID: id,
		Status:       status,
		Address:      address,
		ErrMessage:   errMessage,
	})
	return nil
}

func (s *fakeStore) ListEvents(_ context.Context, id fleet.AllocationID, limit int) ([]fleet.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fleet.Event
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		if s.events[i].AllocationID == id {
			out = append(out, s.events[i])
		}
	}
	return out, nil
}

func (s *fakeStore) BackendsForTenant(_ context.Context) ([]fleet.BackendStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int64{}
	for _, a := range s.allocations {
		counts[a.Backend]++
	}
	var out []fleet.BackendStats
	for name, c := range counts {
		out = append(out, fleet.BackendStats{Name: name, AllocationCount: c})
	}
	return out, nil
}

// fakeFleetStore is a tiny in-memory FleetStore for manager tests. Tests
// seed it with one fleet whose Backend matches the fakeBackend's name; the
// manager resolves req.FleetID against this map before dispatching.
type fakeFleetStore struct {
	mu     sync.Mutex
	next   int64
	byID   map[int64]*fleet.Fleet
	byName map[string]*fleet.Fleet
}

func newFakeFleetStore() *fakeFleetStore {
	return &fakeFleetStore{byID: map[int64]*fleet.Fleet{}, byName: map[string]*fleet.Fleet{}}
}

func (s *fakeFleetStore) seed(projectID int64, name, backend string, cfg map[string]string) *fleet.Fleet {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	f := &fleet.Fleet{ID: s.next, TenantID: 1, ProjectID: projectID, Name: name, Backend: backend, Config: cfg}
	s.byID[s.next] = f
	s.byName[name] = f
	return f
}

func (s *fakeFleetStore) Create(_ context.Context, in fleet.FleetCreate) (*fleet.Fleet, error) {
	return s.seed(in.ProjectID, in.Name, in.Backend, in.Config), nil
}

func (s *fakeFleetStore) GetByID(_ context.Context, id int64) (*fleet.Fleet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.byID[id]; ok {
		return f, nil
	}
	return nil, fleet.ErrFleetNotFound
}

func (s *fakeFleetStore) GetByName(_ context.Context, _ int64, name string) (*fleet.Fleet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.byName[name]; ok {
		return f, nil
	}
	return nil, fleet.ErrFleetNotFound
}

func (s *fakeFleetStore) ListForProject(_ context.Context, projectID int64) ([]*fleet.Fleet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*fleet.Fleet
	for _, f := range s.byID {
		if f.ProjectID == projectID {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *fakeFleetStore) Update(_ context.Context, _ fleet.FleetUpdate) error { return nil }
func (s *fakeFleetStore) SoftDelete(_ context.Context, _ int64) error         { return nil }

// newFakeFleetStoreSeed returns a fleet store seeded with one row whose
// Backend matches backendName, so sampleReq() (which uses FleetID=1)
// resolves cleanly inside the manager.
func newFakeFleetStoreSeed(backendName string) *fakeFleetStore {
	fs := newFakeFleetStore()
	fs.seed(2, "default", backendName, map[string]string{})
	return fs
}

func sampleReq() fleet.AllocationRequest {
	return fleet.AllocationRequest{
		TenantID:  1,
		ProjectID: 2,
		FleetID:   1, // matches the seeded "default" fleet (first row inserted by newTestManager)
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
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Retries: 0, Clock: zeroClock})

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
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Retries: 3, Clock: zeroClock})

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
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Retries: 2, Clock: zeroClock})

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

func TestManager_Allocate_markReadyFailureCleansBackendAndPersistsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	markReadyErr := errors.New("database unavailable")
	cleanupErr := errors.New("backend cleanup failed")
	store := newFakeStore()
	store.markReadyErr = markReadyErr
	store.markReadyCancel = cancel
	var cleanupContextActive, cleanupHasDeadline bool
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "container-orphan", Address: "10.0.0.9:7777"}, nil
		},
		deallocateImpl: func(cleanupCtx context.Context, _ fleet.AllocationID, _ string) error {
			cleanupContextActive = cleanupCtx.Err() == nil
			_, cleanupHasDeadline = cleanupCtx.Deadline()
			return cleanupErr
		},
	}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})

	got, err := mgr.Allocate(ctx, sampleReq())

	require.ErrorIs(t, err, markReadyErr)
	assert.NotErrorIs(t, err, cleanupErr)
	assert.Nil(t, got)
	assert.Equal(t, []fleet.AllocationID{1}, backend.deallocateIDs)
	assert.Equal(t, []string{"container-orphan"}, backend.deallocateRefs)
	assert.True(t, cleanupContextActive)
	assert.True(t, cleanupHasDeadline)
	assert.Equal(t, []fleet.AllocationID{1}, store.markFailedIDs)
	assert.True(t, store.markFailedActive)
	assert.True(t, store.markFailedDeadline)
	persisted, getErr := store.Get(context.Background(), 1)
	require.NoError(t, getErr)
	assert.Equal(t, fleet.StatusFailed, persisted.Status)
	events, eventErr := store.ListEvents(context.Background(), 1, 10)
	require.NoError(t, eventErr)
	require.NotEmpty(t, events)
	assert.Equal(t, fleet.StatusFailed, events[0].Status)
	assert.Contains(t, events[0].ErrMessage, "container-orphan")
	assert.Contains(t, events[0].ErrMessage, cleanupErr.Error())
}

func TestManager_Deallocate_calls_backend_and_releases_row(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "ref-1", Address: "10.0.0.1:7777"}, nil
		},
	}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})
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
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})
	a, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)

	out, err := mgr.Watch(context.Background(), a.ID)
	require.NoError(t, err)

	got := drainStatuses(t, out)
	assert.Equal(t, []fleet.Status{fleet.StatusAllocating, fleet.StatusReady}, got)
}

func TestManager_Allocate_appends_pending_and_ready_events(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "ref", Address: "1.2.3.4:1"}, nil
		},
	}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})
	a, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)

	events, err := mgr.ListEvents(context.Background(), a.ID, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	// Newest first.
	assert.Equal(t, fleet.StatusReady, events[0].Status)
	assert.Equal(t, "1.2.3.4:1", events[0].Address)
	assert.Equal(t, fleet.StatusPending, events[1].Status)
}

func TestManager_Allocate_appends_failed_event_after_exhausting_retries(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return nil, errors.New("backend down")
		},
	}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Retries: 1, Clock: zeroClock})
	_, err := mgr.Allocate(context.Background(), sampleReq())
	require.Error(t, err)

	events, err := mgr.ListEvents(context.Background(), 1, 10)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, fleet.StatusFailed, events[0].Status)
	assert.Contains(t, events[0].ErrMessage, "backend down")
}

func TestManager_List_skips_terminal_when_excluded(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{
		name: "fake",
		allocateImpl: func(_ int) (*fleet.Allocation, error) {
			return &fleet.Allocation{BackendRef: "ref", Address: "1.2.3.4:1"}, nil
		},
	}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})
	a, err := mgr.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	require.NoError(t, mgr.Deallocate(context.Background(), a.ID))

	excluded, total, err := mgr.List(context.Background(), 2, false, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, excluded)
	assert.Equal(t, int64(0), total)

	included, total, err := mgr.List(context.Background(), 2, true, 10, 0)
	require.NoError(t, err)
	require.Len(t, included, 1)
	assert.Equal(t, int64(1), total)
}

func TestManager_BackendsForTenant_returns_distinct_backends(t *testing.T) {
	store := newFakeStore()
	backend := &fakeBackend{name: "docker", allocateImpl: func(_ int) (*fleet.Allocation, error) {
		return &fleet.Allocation{}, nil
	}}
	mgr := fleet.NewManager(store, newFakeFleetStoreSeed(backend.name), backend, fleet.ManagerOptions{Clock: zeroClock})
	_, _ = mgr.Allocate(context.Background(), sampleReq())
	_, _ = mgr.Allocate(context.Background(), sampleReq())
	stats, err := mgr.BackendsForTenant(context.Background())
	require.NoError(t, err)
	require.Len(t, stats, 1)
	assert.Equal(t, "docker", stats[0].Name)
	assert.Equal(t, int64(2), stats[0].AllocationCount)
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
