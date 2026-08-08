//go:build integration

package fleet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
)

// fakeBackend is a minimal fleet.Backend that resolves allocations without a
// real cluster, so these tests exercise Manager + PostgresFleetStore against a
// live Postgres without needing Agones or a plugin subprocess.
type fakeBackend struct{ name string }

func (f fakeBackend) Name() string { return f.name }

func (fakeBackend) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	return &fleet.Allocation{
		BackendRef: "fake-ref-1",
		Address:    "127.0.0.1:7777",
		Status:     fleet.StatusReady,
	}, nil
}

func (fakeBackend) Deallocate(context.Context, fleet.AllocationID, string) error { return nil }

func (fakeBackend) Status(context.Context, fleet.AllocationID, string) (fleet.Status, error) {
	return fleet.StatusAllocated, nil
}

func (fakeBackend) Watch(context.Context, fleet.AllocationID, string) (<-chan fleet.StatusUpdate, error) {
	return nil, fleet.ErrUnsupported
}

func (fakeBackend) HealthCheck(context.Context) error { return nil }

// TestManager_resolves_fleet_then_allocates is the load-bearing assertion for
// the fleet-template feature: an operator-authored fleet row in Postgres
// translates into an allocation whose persisted row records the fleet it came
// from. Exercises PostgresFleetStore.Create, Manager.Allocate (fleet lookup +
// backend dispatch), and the allocation store, against a real Postgres.
func TestManager_resolves_fleet_then_allocates(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)

	mgr := fleet.NewManager(allocStore, fleetStore, fakeBackend{name: "agones"}, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	tmpl, err := fleetStore.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      "doomerang",
		Backend:   "agones",
		Config:    map[string]string{"fleet_name": "doomerang"},
	})
	require.NoError(t, err)

	alloc, err := mgr.Allocate(ctx, fleet.AllocationRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		FleetID:   tmpl.ID,
		Region:    "local",
		Capacity:  1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Deallocate(ctx, alloc.ID) })

	assert.NotEmpty(t, alloc.BackendRef, "backend ref empty — backend never ran")
	assert.NotEmpty(t, alloc.Address, "address empty — backend never returned one")
	assert.Equal(t, fleet.StatusReady, alloc.Status)

	// The persisted row must carry the fleet_id so the control panel can
	// surface "this allocation came from <fleet>" without joining through
	// metadata.
	persisted, err := mgr.Get(ctx, alloc.ID)
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, persisted.FleetID, "allocation row must record the fleet it came from")
}

// TestManager_refuses_fleet_with_mismatched_backend covers the fail-closed
// path: the configured backend is agones, but the fleet row says a plugin —
// Manager.Allocate must refuse and not call the backend.
func TestManager_refuses_fleet_with_mismatched_backend(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)

	mgr := fleet.NewManager(allocStore, fleetStore, fakeBackend{name: "agones"}, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	tmpl, err := fleetStore.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      "wrong-backend",
		Backend:   "plugin:ovh",
		Config:    map[string]string{"flavor": "b2-7"},
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

// TestManager_refuses_missing_fleet covers the boundary where a caller passes
// a fleet_id that has been soft-deleted or never existed.
func TestManager_refuses_missing_fleet(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	fleetStore := fleet.NewPostgresFleetStore(pool)
	allocStore := fleet.NewPostgresStore(pool)

	mgr := fleet.NewManager(allocStore, fleetStore, fakeBackend{name: "agones"}, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	_, err := mgr.Allocate(ctx, fleet.AllocationRequest{
		TenantID: tenantID, ProjectID: projectID, FleetID: 9999,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrFleetNotFound))
}
