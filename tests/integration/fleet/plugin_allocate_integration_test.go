//go:build integration

package fleet_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	fleetplugin "github.com/automoto/gg-scale/internal/fleet/plugin"
)

// TestManager_allocates_on_the_example_plugin_for_stored_backend_names runs
// the shipped reference plugin under Manager. A fleet stored as the bare name
// "plugin" (older rows) and as "plugin:example" both allocate. A fleet of
// another plugin is refused.
func TestManager_allocates_on_the_example_plugin_for_stored_backend_names(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)

	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "ggscale-fleet-example"), "github.com/automoto/gg-scale/examples/ggscale-fleet-example")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build example plugin: %s", out)
	sup, err := fleetplugin.NewSupervisor(fleetplugin.SupervisorConfig{
		Launch: fleetplugin.LaunchConfig{Dir: dir, Name: "example"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sup.Close() })

	mgr := fleet.NewManager(fleet.NewPostgresStore(pool), fleet.NewPostgresFleetStore(pool, nil), sup, fleet.ManagerOptions{
		Clock: func(int) time.Duration { return 0 },
	})

	cases := []struct {
		backend string
		wantErr bool
	}{
		{"plugin", false},
		{"plugin:example", false},
		{"plugin:other", true},
	}
	for _, tc := range cases {
		t.Run(tc.backend, func(t *testing.T) {
			f, err := fleet.NewPostgresFleetStore(pool, nil).CreateFleet(ctx, tenantID, fleet.FleetCreate{
				ProjectID: projectID, Name: "f-" + tc.backend, Backend: tc.backend,
			}, actor)
			require.NoError(t, err)

			alloc, err := mgr.Allocate(ctx, fleet.AllocationRequest{
				TenantID: tenantID, ProjectID: projectID, FleetID: f.ID, Region: "local", Capacity: 1,
			})

			if tc.wantErr {
				assert.ErrorIs(t, err, fleet.ErrFleetBackendMismatch)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = mgr.Deallocate(ctx, alloc.ID) })
			assert.NotEmpty(t, alloc.Address)
		})
	}
}
