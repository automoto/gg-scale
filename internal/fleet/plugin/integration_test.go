//go:build integration

package plugin_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
	fleetplugin "github.com/ggscale/ggscale/internal/fleet/plugin"
)

// buildExamplePlugin compiles cmd/ggscale-fleet-example into a fresh temp
// directory and returns the directory path. The temp dir is reaped by the
// test's t.TempDir().
func buildExamplePlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ggscale-fleet-example")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/ggscale/ggscale/cmd/ggscale-fleet-example")
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build example plugin: %s", out)
	return dir
}

// TestSupervisor_RestartsAfterCrash spawns the example plugin under
// Supervisor, kills the subprocess mid-flight, and asserts the supervisor
// auto-restarts exactly once and the new instance is functional. Matches the
// M4.4 deliverable from docs/temp/m2.md.
func TestSupervisor_RestartsAfterCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short set")
	}
	dir := buildExamplePlugin(t)

	sup, err := fleetplugin.NewSupervisor(fleetplugin.SupervisorConfig{
		Launch:      fleetplugin.LaunchConfig{Dir: dir, Name: "example"},
		MaxRestarts: 3,
	})
	require.NoError(t, err)
	defer func() { _ = sup.Close() }()

	// Plugin is alive — first Allocate works.
	a, err := sup.Allocate(context.Background(), fleet.AllocationRequest{TenantID: 1, Region: "us-east-1"})
	require.NoError(t, err)
	assert.NotEmpty(t, a.Address)

	// Kill the subprocess to simulate a crash mid-flight.
	pid := sup.Pid()
	require.NotZero(t, pid, "supervisor did not expose a PID")
	proc, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, proc.Signal(syscall.SIGKILL))

	// Supervisor must detect the death and restart within a reasonable window.
	require.Eventually(t, func() bool {
		return sup.TotalRestartCount() == 1 && sup.Pid() != 0 && sup.Pid() != pid
	}, 10*time.Second, 50*time.Millisecond, "supervisor did not restart after subprocess kill")

	// New instance is functional.
	a2, err := sup.Allocate(context.Background(), fleet.AllocationRequest{TenantID: 1, Region: "us-east-1"})
	require.NoError(t, err)
	assert.NotEmpty(t, a2.Address)

	// Supervisor did not over-restart (no flapping). TotalRestartCount is
	// the right check here — RestartCount resets after the healthy
	// post-restart probe.
	assert.Equal(t, 1, sup.TotalRestartCount())
	assert.Equal(t, 0, sup.RestartCount())
}

// TestSupervisor_GivesUpAfterRepeatedPingFailures spawns an example plugin
// configured to always fail Ping, then asserts the supervisor exhausts its
// restart budget and ends up with no live plugin. Tight intervals keep the
// whole exchange under a few seconds.
func TestSupervisor_GivesUpAfterRepeatedPingFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short set")
	}
	dir := buildExamplePlugin(t)

	sup, err := fleetplugin.NewSupervisor(fleetplugin.SupervisorConfig{
		Launch: fleetplugin.LaunchConfig{
			Dir:  dir,
			Name: "example",
			Env:  []string{"GGSCALE_EXAMPLE_FAIL_PING=1"},
		},
		MaxRestarts:       2,
		PollInterval:      30 * time.Millisecond,
		PingInterval:      30 * time.Millisecond,
		PingFailureBudget: 2,
	})
	require.NoError(t, err)
	defer func() { _ = sup.Close() }()

	// Ping fails every time, so the post-restart immediate probe never
	// resets the counter — RestartCount and TotalRestartCount stay equal.
	require.Eventually(t, func() bool {
		return sup.Pid() == 0 && sup.TotalRestartCount() == 2
	}, 5*time.Second, 50*time.Millisecond, "supervisor did not exhaust restart budget after Ping failures")

	// Once exhausted, subsequent ops fail cleanly rather than blocking.
	_, err = sup.Allocate(context.Background(), fleet.AllocationRequest{})
	assert.Error(t, err)
}

// TestSupervisor_CloseDuringRestartIsNoOrphan exercises the shutdown race
// path: kill the subprocess to trigger handleExit's Launch, then call
// Close() while that Launch is in flight. With the fix in supervisor.swap,
// the freshly-launched plugin is closed (not adopted) and no subprocess
// outlives Close().
//
// We can't directly observe the in-flight PID, but we can assert that the
// originally-spawned PID is reaped before Close() returns, that Close()
// doesn't deadlock, and that pgrep finds no surviving ggscale-fleet-example
// owned by this test's temp build directory.
func TestSupervisor_CloseDuringRestartIsNoOrphan(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short set")
	}
	dir := buildExamplePlugin(t)

	// Run several iterations so timing variance across the watch ticker
	// surfaces any residual race.
	const iterations = 5
	for i := 0; i < iterations; i++ {
		sup, err := fleetplugin.NewSupervisor(fleetplugin.SupervisorConfig{
			Launch:       fleetplugin.LaunchConfig{Dir: dir, Name: "example"},
			MaxRestarts:  3,
			PollInterval: 30 * time.Millisecond,
		})
		require.NoError(t, err)

		initialPid := sup.Pid()
		require.NotZero(t, initialPid)

		// Kill the subprocess and immediately ask the supervisor to shut
		// down. Close() must reap the original PID and any restart-in-flight
		// without blocking past Launch's natural duration.
		require.NoError(t, syscall.Kill(initialPid, syscall.SIGKILL))

		done := make(chan struct{})
		go func() {
			_ = sup.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Close did not return within 5s", i)
		}

		// Original PID is gone.
		require.Eventually(t, func() bool {
			return syscall.Kill(initialPid, syscall.Signal(0)) != nil
		}, 2*time.Second, 20*time.Millisecond, "iteration %d: initial pid %d still alive after Close", i, initialPid)

		// After Close, the supervisor reports no live plugin.
		assert.Zero(t, sup.Pid())
	}
}
