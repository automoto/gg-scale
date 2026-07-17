package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGrantStore struct {
	mu           sync.Mutex
	syncCalls    int
	renewCalls   int
	renewed      int
	releaseCalls int
	err          error
	allocations  map[int64]int64
}

type blockingReleaseGrantStore struct {
	*fakeGrantStore
	releaseStarted chan struct{}
	releaseAllowed chan struct{}
	blockOnce      sync.Once
}

func (s *blockingReleaseGrantStore) Release(ctx context.Context, req grantRelease) error {
	s.blockOnce.Do(func() {
		close(s.releaseStarted)
		<-s.releaseAllowed
	})
	return s.fakeGrantStore.Release(ctx, req)
}

func (s *fakeGrantStore) Sync(_ context.Context, req grantRequest) (grantResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncCalls++
	if s.err != nil {
		return grantResult{}, s.err
	}
	if s.allocations == nil {
		s.allocations = make(map[int64]int64)
	}
	allocated := max(req.Used, req.Used+req.Requested)
	if allocated > req.Caps.Ceiling {
		allocated = req.Caps.Ceiling
	}
	s.allocations[req.TenantID] = allocated
	return grantResult{
		Allocated: allocated,
		Current:   req.Used,
	}, nil
}

func (s *fakeGrantStore) Release(_ context.Context, req grantRelease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	delete(s.allocations, req.TenantID)
	return s.err
}

func (s *fakeGrantStore) Renew(_ context.Context, req grantRenewRequest) (map[int64]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewCalls++
	s.renewed = len(req.Grants)
	if s.err != nil {
		return nil, s.err
	}
	renewed := make(map[int64]struct{}, len(req.Grants))
	for _, grant := range req.Grants {
		renewed[grant.TenantID] = struct{}{}
	}
	return renewed, nil
}

func (s *fakeGrantStore) calls() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncCalls, s.releaseCalls
}

func (s *fakeGrantStore) renewalCalls() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewCalls, s.renewed
}

func (s *fakeGrantStore) allocation(tenantID int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocations[tenantID]
}

func newTestLeasedCap(t *testing.T, store *fakeGrantStore, now *time.Time) *LeasedConnectionCap {
	t.Helper()
	cap := newLeasedConnectionCap(store, nil, leasedCapOptions{
		Region:        "us-east",
		HolderID:      "app-a/boot-1",
		Lease:         45 * time.Second,
		RenewInterval: -1,
		Now: func() time.Time {
			return *now
		},
	})
	t.Cleanup(func() {
		_ = cap.Close(context.Background())
	})
	return cap
}

func TestLeasedConnectionCap_defaults_the_renewal_interval(t *testing.T) {
	cap := newLeasedConnectionCap(&fakeGrantStore{}, nil, leasedCapOptions{})
	t.Cleanup(func() { require.NoError(t, cap.Close(context.Background())) })

	assert.Equal(t, defaultRenewInterval, cap.renewInterval)
}

func TestLeasedConnectionCap_reuses_local_grant_without_store_round_trip(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	first, err := cap.Acquire(context.Background(), 41, caps)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	second, err := cap.Acquire(context.Background(), 41, caps)
	require.NoError(t, err)
	assert.True(t, second.Allowed)
	syncCalls, _ := store.calls()
	assert.Equal(t, 1, syncCalls)
}

func TestLeasedConnectionCap_requests_another_block_when_local_grant_is_full(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 64, Ceiling: 64}

	for range 33 {
		decision, err := cap.Acquire(context.Background(), 42, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}

	syncCalls, _ := store.calls()
	assert.Equal(t, 2, syncCalls)
}

func TestLeasedConnectionCap_bounds_emergency_admissions_when_store_is_unavailable(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{err: errors.New("database unavailable")}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 5_000, Ceiling: 10_000}

	for range 8 {
		decision, err := cap.Acquire(context.Background(), 43, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
		assert.True(t, decision.Emergency)
	}

	decision, err := cap.Acquire(context.Background(), 43, caps)
	require.NoError(t, err)
	assert.False(t, decision.Allowed)
	assert.Equal(t, CapRejectUnavailable, decision.Reason)
}

func TestLeasedConnectionCap_release_frees_an_emergency_permit(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{err: errors.New("database unavailable")}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 5_000, Ceiling: 10_000}

	for range 8 {
		decision, err := cap.Acquire(context.Background(), 44, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}
	require.NoError(t, cap.Release(context.Background(), 44))

	decision, err := cap.Acquire(context.Background(), 44, caps)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
}

func TestLeasedConnectionCap_does_not_use_an_expired_local_grant(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	decision, err := cap.Acquire(context.Background(), 45, caps)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	now = now.Add(46 * time.Second)

	decision, err = cap.Acquire(context.Background(), 45, caps)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	syncCalls, _ := store.calls()
	assert.Equal(t, 2, syncCalls)
}

func TestLeasedConnectionCap_stops_using_a_local_grant_before_the_database_lease_expires(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	decision, err := cap.Acquire(context.Background(), 49, caps)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	now = now.Add(31 * time.Second)

	decision, err = cap.Acquire(context.Background(), 49, caps)
	require.NoError(t, err)
	assert.True(t, decision.Allowed)
	syncCalls, _ := store.calls()
	assert.Equal(t, 2, syncCalls, "local validity includes a safety margin for clock skew and network delay")
}

func TestLeasedConnectionCap_releases_database_grant_after_last_connection(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	decision, err := cap.Acquire(context.Background(), 46, caps)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	require.NoError(t, cap.Release(context.Background(), 46))

	_, releaseCalls := store.calls()
	assert.Equal(t, 1, releaseCalls)
}

func TestLeasedConnectionCap_reclaims_idle_allocation_while_connections_remain(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 200, Ceiling: 200}

	for range 65 {
		decision, err := cap.Acquire(context.Background(), 50, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}
	for range 60 {
		require.NoError(t, cap.Release(context.Background(), 50))
	}

	assert.Equal(t, int64(63), store.allocation(50), "idle allocation is bounded to at most two blocks above live usage")
	syncCalls, _ := store.calls()
	assert.Equal(t, 4, syncCalls)
}

func TestLeasedConnectionCap_reaps_a_fully_released_tenant(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	decision, err := cap.Acquire(context.Background(), 51, caps)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	require.NoError(t, cap.Release(context.Background(), 51))

	cap.mu.Lock()
	defer cap.mu.Unlock()
	assert.NotContains(t, cap.grants, int64(51))
}

func TestLeasedConnectionCap_reaps_high_cardinality_released_tenants(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	for tenantID := int64(1); tenantID <= 5_000; tenantID++ {
		decision, err := cap.Acquire(context.Background(), tenantID, caps)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
		require.NoError(t, cap.Release(context.Background(), tenantID))
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	assert.Empty(t, cap.grants)
}

func TestLeasedConnectionCap_does_not_reap_a_grant_retained_by_a_waiting_acquire(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &blockingReleaseGrantStore{
		fakeGrantStore: &fakeGrantStore{},
		releaseStarted: make(chan struct{}),
		releaseAllowed: make(chan struct{}),
	}
	cap := newLeasedConnectionCap(store, nil, leasedCapOptions{
		Region: "us-east", HolderID: "app-a/boot-1", RenewInterval: -1,
		Now: func() time.Time { return now },
	})
	t.Cleanup(func() { require.NoError(t, cap.Close(context.Background())) })
	caps := CapLimits{Sustained: 100, Ceiling: 100}
	decision, err := cap.Acquire(context.Background(), 52, caps)
	require.NoError(t, err)
	require.True(t, decision.Allowed)

	released := make(chan error, 1)
	go func() { released <- cap.Release(context.Background(), 52) }()
	<-store.releaseStarted
	type acquireResult struct {
		decision CapDecision
		err      error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		decision, err := cap.Acquire(context.Background(), 52, caps)
		acquired <- acquireResult{decision: decision, err: err}
	}()
	close(store.releaseAllowed)
	require.NoError(t, <-released)
	result := <-acquired
	require.NoError(t, result.err)
	require.True(t, result.decision.Allowed)

	cap.mu.Lock()
	grant := cap.grants[52]
	cap.mu.Unlock()
	require.NotNil(t, grant)
	grant.mu.Lock()
	assert.Equal(t, int64(1), grant.used)
	grant.mu.Unlock()
	require.NoError(t, cap.Release(context.Background(), 52))
}

func TestLeasedConnectionCap_renews_all_active_tenants_in_one_store_call(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := &fakeGrantStore{}
	cap := newTestLeasedCap(t, store, &now)
	caps := CapLimits{Sustained: 100, Ceiling: 100}

	first, err := cap.Acquire(context.Background(), 47, caps)
	require.NoError(t, err)
	require.True(t, first.Allowed)
	second, err := cap.Acquire(context.Background(), 48, caps)
	require.NoError(t, err)
	require.True(t, second.Allowed)
	cap.renewAll()

	renewCalls, renewed := store.renewalCalls()
	assert.Equal(t, 1, renewCalls)
	assert.Equal(t, 2, renewed)
}

func TestGrantBlockSize_is_bounded(t *testing.T) {
	tests := []struct {
		name    string
		ceiling int64
		want    int64
	}{
		{name: "small tenant", ceiling: 4, want: 32},
		{name: "tier zero", ceiling: 10_000, want: 50},
		{name: "tier two", ceiling: 100_000, want: 500},
		{name: "large override", ceiling: 1_000_000, want: 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, grantBlockSize(tt.ceiling))
		})
	}
}

func TestEmergencyAllowance_is_bounded(t *testing.T) {
	tests := []struct {
		name      string
		sustained int64
		want      int64
	}{
		{name: "minimum", sustained: 5_000, want: 8},
		{name: "proportional", sustained: 20_000, want: 20},
		{name: "maximum", sustained: 100_000, want: 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, emergencyAllowance(tt.sustained))
		})
	}
}
