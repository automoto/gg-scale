//go:build integration

package fleet_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	dockerimage "github.com/docker/docker/api/types/image"
	dockernetwork "github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
)

// stubAPI satisfies docker.API for tests that don't drive the backend past
// Manager-level guards. None of its methods are reached in those tests; if
// they ever are, the panic flags the misuse.
type stubAPI struct{}

func (stubAPI) ContainerCreate(context.Context, *dockercontainer.Config, *dockercontainer.HostConfig, *dockernetwork.NetworkingConfig, *ocispec.Platform, string) (dockercontainer.CreateResponse, error) {
	panic("stubAPI.ContainerCreate called")
}
func (stubAPI) ContainerStart(context.Context, string, dockercontainer.StartOptions) error {
	panic("stubAPI.ContainerStart called")
}
func (stubAPI) ContainerStop(context.Context, string, dockercontainer.StopOptions) error {
	panic("stubAPI.ContainerStop called")
}
func (stubAPI) ContainerRemove(context.Context, string, dockercontainer.RemoveOptions) error {
	panic("stubAPI.ContainerRemove called")
}
func (stubAPI) ContainerInspect(context.Context, string) (dockercontainer.InspectResponse, error) {
	panic("stubAPI.ContainerInspect called")
}
func (stubAPI) ImagePull(context.Context, string, dockerimage.PullOptions) (dockerbackend.ImagePullReadCloser, error) {
	return io.NopCloser(nil), nil
}
func (stubAPI) Events(context.Context, dockerevents.ListOptions) (<-chan dockerevents.Message, <-chan error) {
	panic("stubAPI.Events called")
}
func (stubAPI) Ping(context.Context) (types.Ping, error) { return types.Ping{}, nil }

// TestManager_endToEnd_resolves_fleet_then_allocates_real_container is the
// load-bearing assertion for the fleet-template feature: an operator-
// authored fleet row in Postgres translates into a real Docker container
// allocation with the template's image and probe settings. Exercises:
//
//   - PostgresFleetStore.Create  (seed the template)
//   - Manager.Allocate           (looks up fleet, hands config to backend)
//   - docker.Backend.Allocate    (reads req.Config["image|port|probe_*"])
//   - Real Docker daemon         (pulls + runs the container, returns port)
//
// Requires both Postgres (via testcontainers) and a reachable Docker
// daemon. Skipped under -short.
func TestManager_endToEnd_resolves_fleet_then_allocates_real_container(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short set")
	}
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)

	backend, err := dockerbackend.NewFromEnv(dockerbackend.Config{
		PublicIP:     "127.0.0.1",
		ProbeTimeout: 30 * time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, backend.HealthCheck(ctx))

	mgr := fleet.NewManager(allocStore, fleetStore, backend, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	// Seed a docker fleet that points at traefik/whoami (used elsewhere in
	// the docker integration test, so the local daemon should already have
	// it cached — pull_image=true guards a cold CI cache regardless).
	tmpl, err := fleetStore.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      "whoami",
		Backend:   "docker",
		Config: map[string]string{
			"image":      "traefik/whoami:latest",
			"port":       "80",
			"probe_type": "http",
			"probe_path": "/",
			"pull_image": "true",
		},
	})
	require.NoError(t, err)

	start := time.Now()
	alloc, err := mgr.Allocate(ctx, fleet.AllocationRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		FleetID:   tmpl.ID,
		Region:    "local",
		Capacity:  1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Deallocate(ctx, alloc.ID) })

	t.Logf("end-to-end cold start: %s", time.Since(start))
	assert.NotEmpty(t, alloc.BackendRef, "backend ref empty — backend never ran")
	assert.NotEmpty(t, alloc.Address, "address empty — port mapping never resolved")
	assert.Equal(t, fleet.StatusReady, alloc.Status)

	// Reach the running whoami container on the published port.
	resp, err := http.Get("http://" + alloc.Address)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The persisted row must carry the fleet_id so the control panel can
	// surface "this allocation came from <fleet>" without joining through
	// metadata.
	persisted, err := mgr.Get(ctx, alloc.ID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, persisted.FleetID, "allocation row must record the fleet it came from")
}

// TestManager_refuses_fleet_with_mismatched_backend covers the fail-closed
// path: the configured backend is docker, but the fleet row says agones —
// Manager.Allocate must refuse and not call the backend.
func TestManager_refuses_fleet_with_mismatched_backend(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)

	backend, err := dockerbackend.New(dockerbackend.Config{Client: stubAPI{}, PublicIP: "127.0.0.1"})
	require.NoError(t, err)
	mgr := fleet.NewManager(allocStore, fleetStore, backend, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	tmpl, err := fleetStore.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      "wrong-backend",
		Backend:   "agones",
		Config:    map[string]string{"fleet_name": "doomerang"},
	})
	require.NoError(t, err)

	_, err = mgr.Allocate(ctx, fleet.AllocationRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: tmpl.ID,
		Region: "local", Capacity: 1,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrFleetBackendMismatch),
		"manager must refuse a fleet whose backend disagrees with the configured backend")
}

// TestManager_refuses_missing_fleet covers the boundary where a caller
// passes a fleet_id that has been soft-deleted or never existed.
func TestManager_refuses_missing_fleet(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)
	backend, err := dockerbackend.New(dockerbackend.Config{Client: stubAPI{}, PublicIP: "127.0.0.1"})
	require.NoError(t, err)
	mgr := fleet.NewManager(allocStore, fleetStore, backend, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	_, err = mgr.Allocate(ctx, fleet.AllocationRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: 9999,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrFleetNotFound))
}
