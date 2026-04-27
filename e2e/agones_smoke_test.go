//go:build e2e

package e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	versioned "agones.dev/agones/pkg/client/clientset/versioned"
	agonesclient "agones.dev/agones/pkg/client/clientset/versioned/typed/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

func TestAgonesAllocation_assigns_host_port_reachable_via_udp(t *testing.T) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath(t))
	require.NoError(t, err)

	cs, err := versioned.NewForConfig(cfg)
	require.NoError(t, err)

	gsClient := cs.AgonesV1().GameServers("default")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	gs := &agonesv1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ggscale-smoke"},
		Spec: agonesv1.GameServerSpec{
			Ports: []agonesv1.GameServerPort{{
				Name:          "default",
				PortPolicy:    agonesv1.Dynamic,
				ContainerPort: 7654,
				Protocol:      "UDP",
			}},
			Template: simpleGameServerTemplate(),
		},
	}

	_, err = gsClient.Create(ctx, gs, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = gsClient.Delete(context.Background(), "ggscale-smoke", metav1.DeleteOptions{})
	})

	port := waitForReadyPort(ctx, t, gsClient, "ggscale-smoke")
	require.NotZero(t, port)

	echo := dialAndEcho(t, port, "PING\n")
	assert.Contains(t, echo, "ACK")
}

func waitForReadyPort(ctx context.Context, t *testing.T, gsClient agonesclient.GameServerInterface, name string) int32 {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		got, err := gsClient.Get(ctx, name, metav1.GetOptions{})
		if err == nil && got.Status.State == agonesv1.GameServerStateReady && len(got.Status.Ports) > 0 {
			return got.Status.Ports[0].Port
		}
		select {
		case <-ctx.Done():
			t.Fatalf("GameServer %q never reached Ready: %v", name, ctx.Err())
			return 0
		case <-ticker.C:
		}
	}
}

func dialAndEcho(t *testing.T, port int32, msg string) string {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))

	conn, err := net.DialTimeout("udp", addr, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))

	_, err = conn.Write([]byte(msg))
	require.NoError(t, err)

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	return string(buf[:n])
}

func simpleGameServerTemplate() corev1.PodTemplateSpec {
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

func kubeconfigPath(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("KUBECONFIG"); v != "" {
		return v
	}
	abs, err := filepath.Abs(filepath.Join("..", ".k3s", "kubeconfig.yaml"))
	require.NoError(t, err)
	return abs
}
