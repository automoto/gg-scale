//go:build integration

package docker_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
)

// TestBackend_RealDaemon_Allocate_to_Deallocate exercises the full
// container lifecycle against the local Docker daemon. Requires DOCKER_HOST
// (or default unix socket) to be reachable and the daemon to be able to
// pull traefik/whoami — a tiny HTTP echo server on port 80.
func TestBackend_RealDaemon_Allocate_to_Deallocate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short set")
	}

	be, err := dockerbackend.NewFromEnv(dockerbackend.Config{
		PublicIP:     "127.0.0.1",
		ProbeTimeout: 30 * time.Second,
	})
	require.NoError(t, err)

	require.NoError(t, be.HealthCheck(context.Background()))

	start := time.Now()
	got, err := be.Allocate(context.Background(), fleet.AllocationRequest{
		TenantID:  1,
		ProjectID: 1,
		FleetID:   1,
		Backend:   "docker",
		Config: map[string]string{
			"image":      "traefik/whoami:latest",
			"port":       "80",
			"probe_type": "http",
			"probe_path": "/",
			"pull_image": "true",
		},
		Region:   "local",
		Capacity: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = be.Deallocate(context.Background(), 0, got.BackendRef)
	})

	t.Logf("cold start: %s", time.Since(start))
	assert.NotEmpty(t, got.BackendRef)
	assert.NotEmpty(t, got.Address)

	// The address Allocate returns must be reachable.
	resp, err := http.Get("http://" + got.Address)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, be.Deallocate(context.Background(), 0, got.BackendRef))

	// Post-deallocate the port should no longer accept connections.
	conn, dialErr := net.DialTimeout("tcp", got.Address, 500*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("expected dial to %s to fail after Deallocate", got.Address)
	}
}
