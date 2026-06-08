package agones_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
	agonesbackend "github.com/ggscale/ggscale/internal/fleet/agones"
)

// fakeAPI scripts agones responses for the unit tests. Each call records
// its arguments for assertions.
type fakeAPI struct {
	mu              sync.Mutex
	createCalls     int
	deleteCalls     int
	lastSpec        allocationv1.GameServerAllocationSpec
	lastDeletedName string
	createResult    *allocationv1.GameServerAllocation
	createErr       error
	getResult       *agonesv1.GameServer
	getErr          error
	watchEvents     []agonesbackend.GameServerEvent
	pingErr         error
}

func (f *fakeAPI) CreateGameServerAllocation(_ context.Context, _ string, gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastSpec = gsa.Spec
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResult, nil
}

func (f *fakeAPI) DeleteGameServer(_ context.Context, _, name string, _ metav1.DeleteOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	f.lastDeletedName = name
	return nil
}

func (f *fakeAPI) GetGameServer(_ context.Context, _, _ string) (*agonesv1.GameServer, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeAPI) WatchGameServer(_ context.Context, _, _ string) (<-chan agonesbackend.GameServerEvent, error) {
	ch := make(chan agonesbackend.GameServerEvent, len(f.watchEvents))
	for _, ev := range f.watchEvents {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (f *fakeAPI) Ping(_ context.Context) error { return f.pingErr }

func sampleReq() fleet.AllocationRequest {
	return reqWithFleet("doomerang", nil)
}

func reqWithFleet(fleetName string, selectors map[string]string) fleet.AllocationRequest {
	cfg := map[string]string{}
	if fleetName != "" {
		cfg["fleet_name"] = fleetName
	}
	for k, v := range selectors {
		cfg["selector."+k] = v
	}
	return fleet.AllocationRequest{
		TenantID:  1,
		ProjectID: 2,
		FleetID:   42,
		Backend:   "agones",
		Config:    cfg,
		Region:    "us-east-1",
		Capacity:  4,
	}
}

func allocatedResult(gs, addr string, port int32) *allocationv1.GameServerAllocation {
	return &allocationv1.GameServerAllocation{
		Status: allocationv1.GameServerAllocationStatus{
			State:          allocationv1.GameServerAllocationAllocated,
			GameServerName: gs,
			Address:        addr,
			Ports:          []agonesv1.GameServerStatusPort{{Name: "default", Port: port}},
		},
	}
}

func TestBackend_Allocate_creates_allocation_with_fleet_and_region_selectors(t *testing.T) {
	fake := &fakeAPI{
		createResult: allocatedResult("gs-1", "10.0.0.5", 7654),
	}
	be, err := agonesbackend.New(agonesbackend.Config{
		API:       fake,
		Namespace: "ggscale",
	})
	require.NoError(t, err)

	got, err := be.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	// BackendRef encodes the namespace so per-template namespace overrides
	// can be honored on the teardown/observe path.
	assert.Equal(t, "ggscale/gs-1", got.BackendRef)
	assert.Equal(t, "10.0.0.5:7654", got.Address)
	assert.Equal(t, fleet.StatusReady, got.Status)

	require.Len(t, fake.lastSpec.Selectors, 1)
	labels := fake.lastSpec.Selectors[0].MatchLabels
	assert.Equal(t, "doomerang", labels["agones.dev/fleet"])
	assert.Equal(t, "us-east-1", labels["ggscale.region"])
}

func TestBackend_Allocate_populates_protocol_from_gameserver_spec(t *testing.T) {
	cases := []struct {
		name     string
		port     agonesv1.GameServerPort
		expected string
	}{
		{"tcp lowercased", agonesv1.GameServerPort{Protocol: "TCP", ContainerPort: 7373}, "tcp"},
		{"udp lowercased", agonesv1.GameServerPort{Protocol: "UDP", ContainerPort: 7777}, "udp"},
		{"tcpudp lowercased", agonesv1.GameServerPort{Protocol: "TCPUDP", ContainerPort: 7777}, "tcpudp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAPI{
				createResult: allocatedResult("gs-1", "10.0.0.5", 7654),
				getResult: &agonesv1.GameServer{
					Spec: agonesv1.GameServerSpec{
						Ports: []agonesv1.GameServerPort{tc.port},
					},
				},
			}
			be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
			require.NoError(t, err)

			got, err := be.Allocate(context.Background(), sampleReq())
			require.NoError(t, err)
			assert.Equal(t, tc.expected, got.Protocol)
		})
	}
}

func TestBackend_Allocate_leaves_protocol_empty_when_gameserver_lookup_fails(t *testing.T) {
	fake := &fakeAPI{
		createResult: allocatedResult("gs-1", "10.0.0.5", 7654),
		getErr:       errors.New("not found"),
	}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)

	got, err := be.Allocate(context.Background(), sampleReq())
	require.NoError(t, err, "protocol read failure must not fail the allocation; protocol_hint is observability, not contract")
	assert.Empty(t, got.Protocol)
}

func TestBackend_Allocate_returns_error_on_unallocated(t *testing.T) {
	fake := &fakeAPI{
		createResult: &allocationv1.GameServerAllocation{
			Status: allocationv1.GameServerAllocationStatus{State: allocationv1.GameServerAllocationUnAllocated},
		},
	}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)

	_, err = be.Allocate(context.Background(), sampleReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UnAllocated")
}

func TestBackend_Allocate_returns_error_on_missing_address(t *testing.T) {
	fake := &fakeAPI{
		createResult: &allocationv1.GameServerAllocation{
			Status: allocationv1.GameServerAllocationStatus{
				State:          allocationv1.GameServerAllocationAllocated,
				GameServerName: "gs-no-port",
			},
		},
	}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)

	_, err = be.Allocate(context.Background(), sampleReq())
	require.Error(t, err)
}

func TestBackend_Deallocate_deletes_named_gameserver(t *testing.T) {
	fake := &fakeAPI{}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)

	require.NoError(t, be.Deallocate(context.Background(), 1, "gs-42"))
	assert.Equal(t, 1, fake.deleteCalls)
	assert.Equal(t, "gs-42", fake.lastDeletedName)
}

func TestBackend_Deallocate_treats_notfound_as_success(t *testing.T) {
	fake := &fakeAPI{}
	gv := schema.GroupResource{Group: "agones.dev", Resource: "gameservers"}
	fake.deleteCalls = 0
	be, _ := agonesbackend.New(agonesbackend.Config{API: notFoundDeleteAPI{inner: fake, gr: gv}, Namespace: "ggscale"})
	require.NoError(t, be.Deallocate(context.Background(), 1, "missing"))
}

// notFoundDeleteAPI wraps fakeAPI to force Delete to return a typed
// NotFound — exercising the idempotent shutdown path.
type notFoundDeleteAPI struct {
	inner *fakeAPI
	gr    schema.GroupResource
}

func (n notFoundDeleteAPI) CreateGameServerAllocation(ctx context.Context, ns string, gsa *allocationv1.GameServerAllocation) (*allocationv1.GameServerAllocation, error) {
	return n.inner.CreateGameServerAllocation(ctx, ns, gsa)
}
func (n notFoundDeleteAPI) DeleteGameServer(_ context.Context, _, name string, _ metav1.DeleteOptions) error {
	return apierrors.NewNotFound(n.gr, name)
}
func (n notFoundDeleteAPI) GetGameServer(ctx context.Context, ns, name string) (*agonesv1.GameServer, error) {
	return n.inner.GetGameServer(ctx, ns, name)
}
func (n notFoundDeleteAPI) WatchGameServer(ctx context.Context, ns, name string) (<-chan agonesbackend.GameServerEvent, error) {
	return n.inner.WatchGameServer(ctx, ns, name)
}
func (n notFoundDeleteAPI) Ping(ctx context.Context) error { return n.inner.Ping(ctx) }

func TestBackend_Status_maps_agones_states(t *testing.T) {
	table := []struct {
		in   agonesv1.GameServerState
		want fleet.Status
	}{
		{agonesv1.GameServerStateReady, fleet.StatusReady},
		{agonesv1.GameServerStateAllocated, fleet.StatusAllocated},
		{agonesv1.GameServerStateShutdown, fleet.StatusShutdown},
		{agonesv1.GameServerStateError, fleet.StatusFailed},
		{agonesv1.GameServerStateUnhealthy, fleet.StatusFailed},
		{agonesv1.GameServerStateScheduled, fleet.StatusAllocating},
	}
	for _, tc := range table {
		t.Run(string(tc.in), func(t *testing.T) {
			fake := &fakeAPI{getResult: &agonesv1.GameServer{Status: agonesv1.GameServerStatus{State: tc.in}}}
			be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
			require.NoError(t, err)
			got, err := be.Status(context.Background(), 1, "gs-x")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBackend_Watch_pipes_events_through(t *testing.T) {
	fake := &fakeAPI{
		watchEvents: []agonesbackend.GameServerEvent{
			{State: agonesv1.GameServerStateScheduled},
			{State: agonesv1.GameServerStateReady},
			{State: agonesv1.GameServerStateAllocated},
		},
	}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)

	ch, err := be.Watch(context.Background(), 1, "gs-1")
	require.NoError(t, err)

	got := drainStatuses(t, ch)
	assert.Equal(t, []fleet.Status{fleet.StatusAllocating, fleet.StatusReady, fleet.StatusAllocated}, got)
}

func TestBackend_HealthCheck_surfaces_ping_failure(t *testing.T) {
	fake := &fakeAPI{pingErr: errors.New("api down")}
	be, err := agonesbackend.New(agonesbackend.Config{API: fake, Namespace: "ggscale"})
	require.NoError(t, err)
	require.Error(t, be.HealthCheck(context.Background()))
}

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
