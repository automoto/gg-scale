//go:build agones_e2e

package agones_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	versioned "agones.dev/agones/pkg/client/clientset/versioned"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ggscale/ggscale/internal/fleet"
	agonesbackend "github.com/ggscale/ggscale/internal/fleet/agones"
)

// TestBackend_RealCluster_Allocate_to_Deallocate exercises the agones
// backend against a real K3s + Agones cluster. Gated by the agones_e2e
// build tag *and* the AGONES_E2E env var so accidental `go test ./...`
// invocations don't surprise developers without a cluster.
//
// Setup:
//
//	make up-k8s && make agones-install
//	AGONES_E2E=1 go test -tags=agones_e2e ./internal/fleet/agones/
func TestBackend_RealCluster_Allocate_to_Deallocate(t *testing.T) {
	if os.Getenv("AGONES_E2E") == "" {
		t.Skip("AGONES_E2E not set")
	}

	kcfg := kubeconfigOrSkip(t)
	cs, err := newClientset(kcfg)
	require.NoError(t, err)

	const ns = "default"
	const gsName = "ggscale-fleet-smoke"
	const label = "ggscale-smoke"

	// Pre-create a single Ready GameServer the backend can allocate from.
	gsClient := cs.AgonesV1().GameServers(ns)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_ = gsClient.Delete(ctx, gsName, metav1.DeleteOptions{}) // clear any prior run
	created, err := gsClient.Create(ctx, &agonesv1.GameServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:   gsName,
			Labels: map[string]string{"ggscale-suite": label},
		},
		Spec: agonesv1.GameServerSpec{
			Ports: []agonesv1.GameServerPort{{
				Name:          "default",
				PortPolicy:    agonesv1.Dynamic,
				ContainerPort: 7654,
				Protocol:      "UDP",
			}},
			Template: simpleSGSTemplate(),
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = gsClient.Delete(context.Background(), created.Name, metav1.DeleteOptions{}) })

	require.NoError(t, waitForState(ctx, t, gsClient, gsName, agonesv1.GameServerStateReady))

	be, err := agonesbackend.NewFromKubeconfig(agonesbackend.Config{
		Namespace: ns,
	}, kcfg)
	require.NoError(t, err)
	require.NoError(t, be.HealthCheck(ctx))

	got, err := be.Allocate(ctx, fleet.AllocationRequest{
		TenantID:  1,
		ProjectID: 1,
		FleetID:   1,
		Backend:   "agones",
		Config: map[string]string{
			"selector.ggscale-suite": label,
		},
		Region: "",
	})
	require.NoError(t, err)
	assert.Equal(t, fleet.StatusReady, got.Status)
	assert.NotEmpty(t, got.Address)

	require.NoError(t, be.Deallocate(ctx, 0, got.BackendRef))
}

func waitForState(ctx context.Context, t *testing.T, gsClient interface {
	Get(context.Context, string, metav1.GetOptions) (*agonesv1.GameServer, error)
}, name string, want agonesv1.GameServerState) error {
	t.Helper()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		gs, err := gsClient.Get(ctx, name, metav1.GetOptions{})
		if err == nil && gs.Status.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func simpleSGSTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "simple-game-server",
				Image: "gcr.io/agones-images/simple-game-server:0.27",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("20m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
			}},
		},
	}
}

func newClientset(kubeconfig string) (*versioned.Clientset, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return versioned.NewForConfig(cfg)
}

func kubeconfigOrSkip(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", ".k3s", "kubeconfig.yaml"))
	require.NoError(t, err)
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("kubeconfig not found at %s; run make up-k8s && make agones-install", abs)
	}
	return abs
}
