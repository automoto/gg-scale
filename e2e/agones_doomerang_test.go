//go:build e2e

package e2e

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	versioned "agones.dev/agones/pkg/client/clientset/versioned"
	agonesclient "agones.dev/agones/pkg/client/clientset/versioned/typed/agones/v1"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// TestAgones_doomerang_allocate_serves_websocket_and_drains validates the
// full Phase A-D stack end-to-end against a live k3s+Agones cluster.
// Preconditions:
//   - k3s+Agones running (make up-fleet-agones && agones-install).
//   - Doomerang Fleet applied: kubectl apply -f k8s/fleets/doomerang.yaml.
//   - Image buildwrangler/doomerang-server reachable from the cluster.
//
// On non-Linux developer machines where the GameServer's reported
// Status.Address (cluster-internal flannel network) isn't reachable from
// the host, export DOOMERANG_E2E_HOST=<reachable-ip> (Colima users:
// `colima status` reports the address). On Linux CI with direct cluster
// networking the default Status.Address works.
func TestAgones_doomerang_allocate_serves_websocket_and_drains(t *testing.T) {
	kcfg := kubeconfigPathOrSkip(t)
	cfg, err := clientcmd.BuildConfigFromFlags("", kcfg)
	require.NoError(t, err)
	cs, err := versioned.NewForConfig(cfg)
	require.NoError(t, err)

	requireDoomerangFleetReady(t, cs)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	alloc := allocateDoomerang(ctx, t, cs)
	t.Cleanup(func() {
		// Deleting the allocated GameServer triggers the Agones Shutdown
		// state transition the doomerang-server's watcher reacts to.
		_ = cs.AgonesV1().GameServers("default").Delete(
			context.Background(), alloc.gameServerName, metav1.DeleteOptions{})
	})

	host := overrideHost(alloc.address)
	dialAddr := net.JoinHostPort(host, strconv.Itoa(int(alloc.port)))
	t.Logf("dialing doomerang-server at %s (GameServer status reported %s; host override %q)",
		dialAddr, alloc.address, os.Getenv("DOOMERANG_E2E_HOST"))

	wsURL := "ws://" + dialAddr + "/"
	wsCtx, wsCancel := context.WithTimeout(ctx, 5*time.Second)
	defer wsCancel()
	conn, resp, err := websocket.Dial(wsCtx, wsURL, nil)
	require.NoErrorf(t, err, "WebSocket handshake against %s failed: %v (server reachable on this host? colima users export DOOMERANG_E2E_HOST=<colima vm ip>)", dialAddr, err)
	if resp != nil {
		require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode,
			"expected 101 Switching Protocols, got %d", resp.StatusCode)
	}
	// The protocol-level handshake (msgpack JoinRequest -> JoinAccepted)
	// belongs to doomerang's own tests; here we only assert that the
	// allocated address is reachable over TCP and the server speaks WS.
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "smoke complete"))

	// Drain assertion: deleting the GameServer triggers the Agones
	// Shutdown state, the doomerang watcher fires Drain, and the pod
	// exits. The Fleet's terminationGracePeriodSeconds is 60s; allow
	// 90s here for sidecar shutdown overhead too.
	require.NoError(t, cs.AgonesV1().GameServers("default").Delete(
		ctx, alloc.gameServerName, metav1.DeleteOptions{}))
	assertGameServerGone(ctx, t, cs.AgonesV1().GameServers("default"), alloc.gameServerName, 90*time.Second)
}

type allocatedDoomerang struct {
	gameServerName string
	address        string
	port           int32
}

func allocateDoomerang(ctx context.Context, t *testing.T, cs *versioned.Clientset) allocatedDoomerang {
	t.Helper()
	allocClient := cs.AllocationV1().GameServerAllocations("default")
	gsa := &allocationv1.GameServerAllocation{
		Spec: allocationv1.GameServerAllocationSpec{
			Selectors: []allocationv1.GameServerSelector{
				{LabelSelector: metav1.LabelSelector{MatchLabels: map[string]string{
					"agones.dev/fleet": "doomerang",
				}}},
			},
		},
	}
	res, err := allocClient.Create(ctx, gsa, metav1.CreateOptions{})
	require.NoError(t, err, "GameServerAllocation create failed; is the Fleet applied with replicas > 0?")
	require.Equal(t, allocationv1.GameServerAllocationAllocated, res.Status.State,
		"allocation state=%s gs=%q (Fleet may have no Ready replicas)", res.Status.State, res.Status.GameServerName)
	require.NotEmpty(t, res.Status.Address)
	require.NotEmpty(t, res.Status.Ports)
	return allocatedDoomerang{
		gameServerName: res.Status.GameServerName,
		address:        res.Status.Address,
		port:           res.Status.Ports[0].Port,
	}
}

func requireDoomerangFleetReady(t *testing.T, cs *versioned.Clientset) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fleets, err := cs.AgonesV1().Fleets("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=doomerang",
	})
	require.NoError(t, err)
	if len(fleets.Items) == 0 {
		t.Skipf("no doomerang Fleet found; run `kubectl apply -f k8s/fleets/doomerang.yaml`")
	}
	// Wait briefly for at least one Ready replica.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		f, err := cs.AgonesV1().Fleets("default").Get(ctx, fleets.Items[0].Name, metav1.GetOptions{})
		require.NoError(t, err)
		if f.Status.ReadyReplicas > 0 {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("doomerang Fleet has 0 Ready replicas after 60s; check `kubectl get gs -o wide`")
}

func assertGameServerGone(ctx context.Context, t *testing.T, gsClient agonesclient.GameServerInterface, name string, timeout time.Duration) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		gs, err := gsClient.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		require.NoError(t, err)
		select {
		case <-deadline.C:
			t.Fatalf("GameServer %q still present after %v (state=%s); drain path didn't terminate the pod in time",
				name, timeout, gs.Status.State)
		case <-ticker.C:
		}
	}
}

// overrideHost returns DOOMERANG_E2E_HOST when set, otherwise the
// reported Status.Address. The override exists for developer machines
// where the cluster-internal address isn't reachable from the host
// (Colima on macOS being the canonical case).
func overrideHost(reported string) string {
	if v := os.Getenv("DOOMERANG_E2E_HOST"); v != "" {
		return v
	}
	return reported
}
