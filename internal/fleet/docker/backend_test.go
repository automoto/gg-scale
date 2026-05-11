package docker_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	dockerimage "github.com/docker/docker/api/types/image"
	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
)

// fakeAPI captures calls and returns scripted responses.
type fakeAPI struct {
	mu              sync.Mutex
	createCount     int
	startCount      int
	stopCount       int
	removeCount     int
	lastCreateCfg   *dockercontainer.Config
	inspectResponse dockercontainer.InspectResponse
	eventsCh        chan dockerevents.Message
	eventsErr       chan error
	pingErr         error
}

func (f *fakeAPI) ContainerCreate(_ context.Context, cfg *dockercontainer.Config, _ *dockercontainer.HostConfig, _ *dockernetwork.NetworkingConfig, _ *ocispec.Platform, _ string) (dockercontainer.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount++
	f.lastCreateCfg = cfg
	return dockercontainer.CreateResponse{ID: "container-abc"}, nil
}

func (f *fakeAPI) ContainerStart(_ context.Context, _ string, _ dockercontainer.StartOptions) error {
	f.mu.Lock()
	f.startCount++
	f.mu.Unlock()
	return nil
}

func (f *fakeAPI) ContainerStop(_ context.Context, _ string, _ dockercontainer.StopOptions) error {
	f.mu.Lock()
	f.stopCount++
	f.mu.Unlock()
	return nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, _ string, _ dockercontainer.RemoveOptions) error {
	f.mu.Lock()
	f.removeCount++
	f.mu.Unlock()
	return nil
}

func (f *fakeAPI) ContainerInspect(_ context.Context, _ string) (dockercontainer.InspectResponse, error) {
	return f.inspectResponse, nil
}

func (f *fakeAPI) ImagePull(_ context.Context, _ string, _ dockerimage.PullOptions) (dockerbackend.ImagePullReadCloser, error) {
	return discardCloser{}, nil
}

func (f *fakeAPI) Events(_ context.Context, _ dockerevents.ListOptions) (<-chan dockerevents.Message, <-chan error) {
	if f.eventsCh == nil {
		f.eventsCh = make(chan dockerevents.Message)
		close(f.eventsCh)
	}
	if f.eventsErr == nil {
		f.eventsErr = make(chan error)
	}
	return f.eventsCh, f.eventsErr
}

func (f *fakeAPI) Ping(_ context.Context) (types.Ping, error) {
	if f.pingErr != nil {
		return types.Ping{}, f.pingErr
	}
	return types.Ping{APIVersion: "1.43"}, nil
}

type discardCloser struct{}

func (discardCloser) Read(p []byte) (int, error) { return 0, nil }
func (discardCloser) Close() error               { return nil }

func reservePort(t *testing.T) (host string, port int, keep net.Listener) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port, lis
}

func sampleReq() fleet.AllocationRequest {
	return fleet.AllocationRequest{TenantID: 7, ProjectID: 11, Region: "local", Capacity: 4}
}

func inspectWithPortBinding(containerPort, hostIP string, hostPort int) dockercontainer.InspectResponse {
	ns := &dockercontainer.NetworkSettings{}
	ns.Ports = nat.PortMap{
		nat.Port(containerPort): []nat.PortBinding{
			{HostIP: hostIP, HostPort: strconv.Itoa(hostPort)},
		},
	}
	return dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
			ID:    "container-abc",
			State: &dockercontainer.State{Running: true},
		},
		NetworkSettings: ns,
	}
}

func TestBackend_Allocate_creates_starts_and_returns_host_address(t *testing.T) {
	host, port, keep := reservePort(t)
	defer keep.Close()

	fake := &fakeAPI{}
	fake.inspectResponse = inspectWithPortBinding("7777/tcp", host, port)

	be, err := dockerbackend.New(dockerbackend.Config{
		Client:       fake,
		Image:        "ggscale/echo:latest",
		Port:         7777,
		ProbeType:    "tcp",
		PublicIP:     host,
		ProbeTimeout: 2 * time.Second,
	})
	require.NoError(t, err)

	got, err := be.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Equal(t, 1, fake.createCount)
	assert.Equal(t, 1, fake.startCount)
	assert.Equal(t, "container-abc", got.BackendRef)
	assert.Equal(t, net.JoinHostPort(host, strconv.Itoa(port)), got.Address)
	assert.Equal(t, "7", fake.lastCreateCfg.Labels["ggscale.tenant_id"])
	assert.Equal(t, "11", fake.lastCreateCfg.Labels["ggscale.project_id"])
	assert.Equal(t, "ggscale.fleet", fake.lastCreateCfg.Labels["ggscale.managed_by"])
}

func TestBackend_Allocate_cleans_up_when_probe_never_succeeds(t *testing.T) {
	host, port, keep := reservePort(t)
	keep.Close() // immediately free the port so dials fail

	fake := &fakeAPI{}
	fake.inspectResponse = inspectWithPortBinding("7777/tcp", host, port)

	be, err := dockerbackend.New(dockerbackend.Config{
		Client:       fake,
		Image:        "ggscale/echo:latest",
		Port:         7777,
		ProbeType:    "tcp",
		PublicIP:     host,
		ProbeTimeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	_, err = be.Allocate(context.Background(), sampleReq())
	require.Error(t, err)
	assert.GreaterOrEqual(t, fake.removeCount, 1, "container must be force-removed on probe failure")
}

func TestBackend_Allocate_uses_http_probe_when_configured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	host, port := mustSplitHostPort(t, srv.URL)

	fake := &fakeAPI{}
	fake.inspectResponse = inspectWithPortBinding("7777/tcp", host, port)

	be, err := dockerbackend.New(dockerbackend.Config{
		Client:       fake,
		Image:        "ggscale/echo:latest",
		Port:         7777,
		ProbeType:    "http",
		ProbePath:    "/healthz",
		PublicIP:     host,
		ProbeTimeout: 2 * time.Second,
	})
	require.NoError(t, err)

	got, err := be.Allocate(context.Background(), sampleReq())
	require.NoError(t, err)
	assert.Equal(t, net.JoinHostPort(host, strconv.Itoa(port)), got.Address)
}

func TestBackend_Deallocate_stops_and_removes(t *testing.T) {
	fake := &fakeAPI{}
	be, err := dockerbackend.New(dockerbackend.Config{Client: fake, Image: "ggscale/echo:latest", Port: 7777})
	require.NoError(t, err)

	require.NoError(t, be.Deallocate(context.Background(), fleet.AllocationID(1), "container-abc"))
	assert.Equal(t, 1, fake.stopCount)
	assert.Equal(t, 1, fake.removeCount)
}

func TestBackend_HealthCheck_pings_daemon(t *testing.T) {
	fake := &fakeAPI{}
	be, err := dockerbackend.New(dockerbackend.Config{Client: fake, Image: "ggscale/echo:latest", Port: 7777})
	require.NoError(t, err)
	require.NoError(t, be.HealthCheck(context.Background()))

	fake.pingErr = errors.New("daemon down")
	require.Error(t, be.HealthCheck(context.Background()))
}

func TestBackend_Watch_translates_die_event_to_shutdown(t *testing.T) {
	fake := &fakeAPI{
		eventsCh:  make(chan dockerevents.Message, 1),
		eventsErr: make(chan error, 1),
	}
	be, err := dockerbackend.New(dockerbackend.Config{Client: fake, Image: "ggscale/echo:latest", Port: 7777})
	require.NoError(t, err)

	ch, err := be.Watch(context.Background(), fleet.AllocationID(1), "container-abc")
	require.NoError(t, err)

	fake.eventsCh <- dockerevents.Message{
		Type:   dockerevents.ContainerEventType,
		Action: dockerevents.ActionDie,
		Actor:  dockerevents.Actor{ID: "container-abc"},
	}
	close(fake.eventsCh)

	select {
	case got := <-ch:
		assert.Equal(t, fleet.StatusShutdown, got.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("expected status update from Watch")
	}
}

func mustSplitHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return u.Hostname(), port
}
