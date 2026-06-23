package plugin

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/ggscale/ggscale/internal/fleet"
	fleetpb "github.com/ggscale/ggscale/internal/fleet/plugin/proto"
)

// ── translation tables ──────────────────────────────────────────────────────

func TestStatusRoundTrip(t *testing.T) {
	all := []fleet.Status{
		fleet.StatusPending,
		fleet.StatusAllocating,
		fleet.StatusReady,
		fleet.StatusAllocated,
		fleet.StatusDraining,
		fleet.StatusShutdown,
		fleet.StatusFailed,
	}
	for _, s := range all {
		t.Run(s.String(), func(t *testing.T) {
			got := statusFromProto(statusToProto(s))

			assert.Equal(t, s, got)
		})
	}
}

func TestStatusFromProtoUnspecifiedReturnsFailed(t *testing.T) {
	got := statusFromProto(fleetpb.AllocationStatus_ALLOCATION_STATUS_UNSPECIFIED)

	assert.Equal(t, fleet.StatusFailed, got)
}

func TestReqToProtoPropagatesAllFields(t *testing.T) {
	in := fleet.AllocationRequest{
		TenantID:  7,
		ProjectID: 11,
		Region:    "us-east-1",
		GameMode:  "ranked",
		Capacity:  4,
		Labels:    map[string]string{"map": "tundra"},
	}

	got := reqToProto(in)

	assert.Equal(t, int64(7), got.GetTenantId())
	assert.Equal(t, int64(11), got.GetProjectId())
	assert.Equal(t, "us-east-1", got.GetRegion())
	assert.Equal(t, "ranked", got.GetGameMode())
	assert.Equal(t, int32(4), got.GetCapacity())
	assert.Equal(t, "tundra", got.GetLabels()["map"])
}

func TestReqToProtoClampsOverflowCapacityToZero(t *testing.T) {
	in := fleet.AllocationRequest{Capacity: math.MaxInt32 + 1}

	got := reqToProto(in)

	assert.Equal(t, int32(0), got.GetCapacity())
}

func TestReqToProtoClampsNegativeCapacityToZero(t *testing.T) {
	in := fleet.AllocationRequest{Capacity: -5}

	got := reqToProto(in)

	assert.Equal(t, int32(0), got.GetCapacity())
}

func TestAllocRoundTripPreservesFields(t *testing.T) {
	in := &fleet.Allocation{
		ID: 42, TenantID: 1, ProjectID: 2,
		Backend: "ovh", BackendRef: "instance-xyz",
		Region: "eu-1", Address: "10.0.0.5:7777",
		Status:   fleet.StatusReady,
		Metadata: map[string]string{"flavor": "eco"},
	}

	got := allocFromProto(allocToProto(in))

	assert.Equal(t, in, got)
}

func TestErrToStatusMapsSentinels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"not_found_maps_back", fleet.ErrNotFound, fleet.ErrNotFound},
		{"unsupported_maps_back", fleet.ErrUnsupported, fleet.ErrUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := errFromStatus(errToStatus(tc.err))

			assert.True(t, errors.Is(got, tc.want), "expected %v, got %v", tc.want, got)
		})
	}
}

func TestErrToStatusNilStaysNil(t *testing.T) {
	assert.NoError(t, errToStatus(nil))
	assert.NoError(t, errFromStatus(nil))
}

func TestErrToStatusGenericPreservesMessage(t *testing.T) {
	got := errFromStatus(errToStatus(errors.New("boom")))

	require.Error(t, got)
	assert.Contains(t, got.Error(), "boom")
}

// ── round-trip through gRPC (bufconn, no subprocess) ────────────────────────
//
// These tests stand up the FleetBackend service on an in-memory listener and
// drive it with the host-side grpcClient. This exercises the full translation
// layer (server + client + streaming) without involving hashicorp/go-plugin's
// subprocess machinery — that gets covered by the integration test against
// the reference plugin binary.

type fakeBackend struct {
	mu sync.Mutex

	name        string
	allocate    func(fleet.AllocationRequest) (*fleet.Allocation, error)
	deallocate  func(fleet.AllocationID, string) error
	statusFn    func() (fleet.Status, error)
	watchFn     func() (<-chan fleet.StatusUpdate, error)
	healthCheck func() error

	deallocCalls []fleet.AllocationID
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Allocate(_ context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	return f.allocate(req)
}

func (f *fakeBackend) Deallocate(_ context.Context, id fleet.AllocationID, ref string) error {
	f.mu.Lock()
	f.deallocCalls = append(f.deallocCalls, id)
	f.mu.Unlock()
	if f.deallocate != nil {
		return f.deallocate(id, ref)
	}
	return nil
}

func (f *fakeBackend) Status(_ context.Context, _ fleet.AllocationID, _ string) (fleet.Status, error) {
	return f.statusFn()
}

func (f *fakeBackend) Watch(_ context.Context, _ fleet.AllocationID, _ string) (<-chan fleet.StatusUpdate, error) {
	return f.watchFn()
}

func (f *fakeBackend) HealthCheck(_ context.Context) error {
	if f.healthCheck == nil {
		return nil
	}
	return f.healthCheck()
}

func dialBufconn(t *testing.T, impl fleet.Backend) (*grpcClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	fleetpb.RegisterFleetBackendServer(srv, newGRPCServer(impl))
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
	)
	require.NoError(t, err)

	c := newGRPCClient(context.Background(), fleetpb.NewFleetBackendClient(conn))
	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
	}
	return c, cleanup
}

func TestRoundTripAllocate(t *testing.T) {
	wantAlloc := &fleet.Allocation{
		ID: 99, TenantID: 1, ProjectID: 2,
		Backend: "fake", BackendRef: "ref-1",
		Region: "us-east-1", Address: "1.2.3.4:80",
		Status: fleet.StatusReady,
	}
	impl := &fakeBackend{
		allocate: func(req fleet.AllocationRequest) (*fleet.Allocation, error) {
			assert.Equal(t, int64(1), req.TenantID)
			assert.Equal(t, "ranked", req.GameMode)
			return wantAlloc, nil
		},
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	got, err := c.Allocate(context.Background(), fleet.AllocationRequest{
		TenantID: 1, GameMode: "ranked", Capacity: 4,
	})

	require.NoError(t, err)
	assert.Equal(t, wantAlloc, got)
}

func TestRoundTripAllocateNotFoundSentinelSurfaces(t *testing.T) {
	impl := &fakeBackend{
		allocate: func(fleet.AllocationRequest) (*fleet.Allocation, error) {
			return nil, fleet.ErrNotFound
		},
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	_, err := c.Allocate(context.Background(), fleet.AllocationRequest{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrNotFound))
}

func TestRoundTripStatus(t *testing.T) {
	impl := &fakeBackend{
		statusFn: func() (fleet.Status, error) { return fleet.StatusAllocated, nil },
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	got, err := c.Status(context.Background(), 5, "ref")

	require.NoError(t, err)
	assert.Equal(t, fleet.StatusAllocated, got)
}

func TestRoundTripDeallocateInvokesBackend(t *testing.T) {
	impl := &fakeBackend{}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	err := c.Deallocate(context.Background(), 7, "ref-7")

	require.NoError(t, err)
	assert.Equal(t, []fleet.AllocationID{7}, impl.deallocCalls)
}

func TestRoundTripWatchPipesUpdatesAndClosesOnEOF(t *testing.T) {
	src := make(chan fleet.StatusUpdate, 3)
	src <- fleet.StatusUpdate{Status: fleet.StatusAllocating}
	src <- fleet.StatusUpdate{Status: fleet.StatusReady, Address: "1.2.3.4:80"}
	close(src)

	impl := &fakeBackend{
		watchFn: func() (<-chan fleet.StatusUpdate, error) { return src, nil },
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := c.Watch(ctx, 1, "ref")
	require.NoError(t, err)

	got := drainStatuses(t, ch)
	require.Len(t, got, 2)
	assert.Equal(t, fleet.StatusAllocating, got[0].Status)
	assert.Equal(t, fleet.StatusReady, got[1].Status)
	assert.Equal(t, "1.2.3.4:80", got[1].Address)
}

func TestRoundTripWatchPropagatesErrorMessage(t *testing.T) {
	src := make(chan fleet.StatusUpdate, 1)
	src <- fleet.StatusUpdate{Status: fleet.StatusFailed, Err: errors.New("oom")}
	close(src)

	impl := &fakeBackend{
		watchFn: func() (<-chan fleet.StatusUpdate, error) { return src, nil },
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := c.Watch(ctx, 1, "ref")
	require.NoError(t, err)

	got := drainStatuses(t, ch)
	require.Len(t, got, 1)
	assert.Equal(t, fleet.StatusFailed, got[0].Status)
	require.Error(t, got[0].Err)
	assert.Contains(t, got[0].Err.Error(), "oom")
}

func TestRoundTripHealthCheckSurfacesError(t *testing.T) {
	impl := &fakeBackend{
		healthCheck: func() error { return errors.New("dead") },
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	err := c.HealthCheck(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dead")
}

func TestRoundTripNameIsCached(t *testing.T) {
	calls := 0
	impl := &fakeBackend{name: "fake"}
	impl.allocate = func(fleet.AllocationRequest) (*fleet.Allocation, error) {
		calls++
		return &fleet.Allocation{}, nil
	}
	c, cleanup := dialBufconn(t, impl)
	defer cleanup()

	first := c.Name()
	second := c.Name()

	assert.Equal(t, "fake", first)
	assert.Equal(t, first, second)
}

func TestRoundTripPingReturnsNoError(t *testing.T) {
	c, cleanup := dialBufconn(t, &fakeBackend{})
	defer cleanup()

	err := c.Ping(context.Background())

	assert.NoError(t, err)
}

func drainStatuses(t *testing.T, ch <-chan fleet.StatusUpdate) []fleet.StatusUpdate {
	t.Helper()
	var out []fleet.StatusUpdate
	timeout := time.After(2 * time.Second)
	for {
		select {
		case upd, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, upd)
		case <-timeout:
			t.Fatalf("watch channel did not close in time; collected %d updates", len(out))
		}
	}
}
