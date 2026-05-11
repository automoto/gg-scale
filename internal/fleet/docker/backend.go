// Package docker implements a fleet.Backend that runs game-server instances
// as Docker containers on the host's daemon. It's the default backend for
// the single-VPS self-host story — no Kubernetes, no Agones, no networking
// gymnastics beyond a port publish.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	dockernetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/ggscale/ggscale/internal/fleet"
)

// API is the slice of the Docker client surface this backend depends on.
// Real callers pass a *client.Client; tests swap in an in-memory fake.
type API interface {
	ContainerCreate(ctx context.Context, config *dockercontainer.Config, hostConfig *dockercontainer.HostConfig, networkingConfig *dockernetwork.NetworkingConfig, platform *ocispec.Platform, containerName string) (dockercontainer.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options dockercontainer.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options dockercontainer.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options dockercontainer.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (dockercontainer.InspectResponse, error)
	ImagePull(ctx context.Context, refStr string, options dockerimage.PullOptions) (ImagePullReadCloser, error)
	Events(ctx context.Context, options dockerevents.ListOptions) (<-chan dockerevents.Message, <-chan error)
	Ping(ctx context.Context) (types.Ping, error)
}

// ImagePullReadCloser is an alias for io.ReadCloser declared in this package
// so the API interface above does not pull in an io import on consumers that
// only need the dependency for its method-set.
type ImagePullReadCloser = io.ReadCloser

// Config wires the backend at startup. Port is the *container* port the
// game server listens on; the daemon publishes it to a dynamic host port
// which Backend.Allocate returns to the matchmaker.
type Config struct {
	// Client is the Docker daemon adapter. Pass *client.Client in production.
	Client API
	// Image is the OCI image reference, e.g. "ghcr.io/example/gs:1.2.3".
	Image string
	// Port is the in-container listening port the game server binds.
	Port int
	// ProbeType is "tcp" or "http". An empty value disables the readiness
	// probe entirely (the container is considered ready as soon as Docker
	// reports it running — fine for local dev, not for production).
	ProbeType string
	// ProbePath is the HTTP path to GET when ProbeType is "http".
	ProbePath string
	// ProbeTimeout bounds how long Allocate waits for the probe to succeed.
	// Default 30s when zero.
	ProbeTimeout time.Duration
	// PublicIP is the host or IP returned to game clients. Required when
	// the daemon is not on localhost or when the published port isn't
	// reachable on 127.0.0.1.
	PublicIP string
	// PullImage controls whether Allocate pulls the image before
	// ContainerCreate. Default false — operators bake images into hosts
	// for predictable cold-start latency.
	PullImage bool
}

// Backend allocates game-server containers via Docker.
type Backend struct {
	cfg          Config
	probeTimeout time.Duration
}

// NewFromEnv is the production constructor; it builds a Docker client from
// the standard DOCKER_HOST/DOCKER_TLS_VERIFY environment variables.
func NewFromEnv(cfg Config) (*Backend, error) {
	if cfg.Client == nil {
		c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("docker: client: %w", err)
		}
		cfg.Client = clientAdapter{c}
	}
	return New(cfg)
}

// New is a thin wrapper that validates the config and applies defaults.
// Tests use this entry point so they can inject a fake API.
func New(cfg Config) (*Backend, error) {
	if cfg.Client == nil {
		return nil, errors.New("docker: Client is required")
	}
	if cfg.Image == "" {
		return nil, errors.New("docker: Image is required")
	}
	if cfg.Port <= 0 {
		return nil, errors.New("docker: Port must be > 0")
	}
	timeout := cfg.ProbeTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Backend{cfg: cfg, probeTimeout: timeout}, nil
}

// Name returns the backend identifier persisted on every allocation row.
func (b *Backend) Name() string { return "docker" }

// Allocate creates, starts, and probes a container. On any failure past
// ContainerCreate the container is force-removed so a failed Allocate
// leaves no orphan resources behind.
func (b *Backend) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	if b.cfg.PullImage {
		if err := b.pull(ctx); err != nil {
			return nil, err
		}
	}

	containerPort, err := nat.NewPort("tcp", strconv.Itoa(b.cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("docker: port: %w", err)
	}

	created, err := b.cfg.Client.ContainerCreate(ctx,
		&dockercontainer.Config{
			Image:        b.cfg.Image,
			ExposedPorts: nat.PortSet{containerPort: struct{}{}},
			Labels: map[string]string{
				"ggscale.managed_by": "ggscale.fleet",
				"ggscale.tenant_id":  strconv.FormatInt(req.TenantID, 10),
				"ggscale.project_id": strconv.FormatInt(req.ProjectID, 10),
				"ggscale.region":     req.Region,
				"ggscale.game_mode":  req.GameMode,
			},
		},
		&dockercontainer.HostConfig{
			PortBindings: nat.PortMap{
				containerPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}},
			},
			AutoRemove: false,
		},
		nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("docker: container create: %w", err)
	}

	if err := b.cfg.Client.ContainerStart(ctx, created.ID, dockercontainer.StartOptions{}); err != nil {
		b.forceRemove(context.Background(), created.ID)
		return nil, fmt.Errorf("docker: container start: %w", err)
	}

	address, err := b.resolveAddress(ctx, created.ID, containerPort)
	if err != nil {
		b.forceRemove(context.Background(), created.ID)
		return nil, err
	}

	if err := b.probe(ctx, address); err != nil {
		b.forceRemove(context.Background(), created.ID)
		return nil, err
	}

	return &fleet.Allocation{
		BackendRef: created.ID,
		Address:    address,
		Status:     fleet.StatusReady,
	}, nil
}

// Deallocate stops the container with a short grace period and removes it.
// Force-remove handles the case where Stop times out (e.g. PID 1 ignores
// SIGTERM); a hung container would otherwise wedge the allocation row.
func (b *Backend) Deallocate(ctx context.Context, _ fleet.AllocationID, backendRef string) error {
	timeout := 10
	if err := b.cfg.Client.ContainerStop(ctx, backendRef, dockercontainer.StopOptions{Timeout: &timeout}); err != nil {
		// fall through to remove — best effort on shutdown
		_ = err
	}
	if err := b.cfg.Client.ContainerRemove(ctx, backendRef, dockercontainer.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("docker: remove: %w", err)
	}
	return nil
}

// Status maps Docker's container state into the fleet lifecycle.
func (b *Backend) Status(ctx context.Context, _ fleet.AllocationID, backendRef string) (fleet.Status, error) {
	resp, err := b.cfg.Client.ContainerInspect(ctx, backendRef)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fleet.StatusShutdown, nil
		}
		return fleet.StatusFailed, fmt.Errorf("docker: inspect: %w", err)
	}
	if resp.State == nil {
		return fleet.StatusFailed, nil
	}
	switch {
	case resp.State.Running:
		return fleet.StatusReady, nil
	case resp.State.Dead, resp.State.OOMKilled:
		return fleet.StatusFailed, nil
	case resp.State.Status == "exited":
		return fleet.StatusShutdown, nil
	default:
		return fleet.StatusAllocating, nil
	}
}

// Watch subscribes to the daemon event stream filtered to the container and
// translates each event to a fleet.StatusUpdate. The returned channel
// closes when the daemon ends the stream or ctx is cancelled.
func (b *Backend) Watch(ctx context.Context, _ fleet.AllocationID, backendRef string) (<-chan fleet.StatusUpdate, error) {
	args := filters.NewArgs()
	args.Add("type", string(dockerevents.ContainerEventType))
	args.Add("container", backendRef)

	msgs, errs := b.cfg.Client.Events(ctx, dockerevents.ListOptions{Filters: args})
	out := make(chan fleet.StatusUpdate, 4)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				if u, emit := translateEvent(msg); emit {
					select {
					case out <- u:
					case <-ctx.Done():
						return
					}
				}
			case err, ok := <-errs:
				if !ok || err == nil {
					return
				}
				select {
				case out <- fleet.StatusUpdate{Status: fleet.StatusFailed, Err: err}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out, nil
}

// HealthCheck pings the daemon.
func (b *Backend) HealthCheck(ctx context.Context) error {
	_, err := b.cfg.Client.Ping(ctx)
	if err != nil {
		return fmt.Errorf("docker: ping: %w", err)
	}
	return nil
}

func (b *Backend) pull(ctx context.Context) error {
	rc, err := b.cfg.Client.ImagePull(ctx, b.cfg.Image, dockerimage.PullOptions{})
	if err != nil {
		return fmt.Errorf("docker: pull: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("docker: drain pull stream: %w", err)
	}
	return nil
}

func (b *Backend) resolveAddress(ctx context.Context, containerID string, containerPort nat.Port) (string, error) {
	resp, err := b.cfg.Client.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("docker: inspect: %w", err)
	}
	if resp.NetworkSettings == nil {
		return "", errors.New("docker: container has no NetworkSettings")
	}
	bindings, ok := resp.NetworkSettings.Ports[containerPort]
	if !ok || len(bindings) == 0 {
		return "", fmt.Errorf("docker: no host binding for %s", containerPort)
	}
	binding := bindings[0]
	host := b.cfg.PublicIP
	if host == "" {
		host = binding.HostIP
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, binding.HostPort), nil
}

func (b *Backend) probe(ctx context.Context, address string) error {
	if b.cfg.ProbeType == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()

	deadline := time.NewTimer(b.probeTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if b.probeOnce(probeCtx, address) {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("docker: probe %s did not succeed within %s", address, b.probeTimeout)
		case <-ticker.C:
		}
	}
}

func (b *Backend) probeOnce(ctx context.Context, address string) bool {
	switch strings.ToLower(b.cfg.ProbeType) {
	case "tcp":
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	case "http":
		path := b.cfg.ProbePath
		if path == "" {
			path = "/"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			return false
		}
		c := http.Client{Timeout: 500 * time.Millisecond}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	default:
		return true
	}
}

func (b *Backend) forceRemove(ctx context.Context, containerID string) {
	timeout := 5
	_ = b.cfg.Client.ContainerStop(ctx, containerID, dockercontainer.StopOptions{Timeout: &timeout})
	_ = b.cfg.Client.ContainerRemove(ctx, containerID, dockercontainer.RemoveOptions{Force: true})
}

func translateEvent(msg dockerevents.Message) (fleet.StatusUpdate, bool) {
	switch msg.Action {
	case dockerevents.ActionStart:
		return fleet.StatusUpdate{Status: fleet.StatusAllocating}, true
	case dockerevents.ActionHealthStatusRunning, dockerevents.ActionHealthStatusHealthy:
		return fleet.StatusUpdate{Status: fleet.StatusReady}, true
	case dockerevents.ActionDie, dockerevents.ActionStop, dockerevents.ActionKill:
		return fleet.StatusUpdate{Status: fleet.StatusShutdown}, true
	case dockerevents.ActionOOM:
		return fleet.StatusUpdate{Status: fleet.StatusFailed, Err: errors.New("docker: container OOM killed")}, true
	default:
		return fleet.StatusUpdate{}, false
	}
}

// clientAdapter narrows *client.Client to API. The two diverge on
// ImagePull's return type (concrete io.ReadCloser vs aliased
// ImagePullReadCloser), so we need a trivial wrapper. Other methods are
// pass-through.
type clientAdapter struct {
	c *client.Client
}

func (a clientAdapter) ContainerCreate(ctx context.Context, config *dockercontainer.Config, host *dockercontainer.HostConfig, net *dockernetwork.NetworkingConfig, plat *ocispec.Platform, name string) (dockercontainer.CreateResponse, error) {
	return a.c.ContainerCreate(ctx, config, host, net, plat, name)
}
func (a clientAdapter) ContainerStart(ctx context.Context, id string, o dockercontainer.StartOptions) error {
	return a.c.ContainerStart(ctx, id, o)
}
func (a clientAdapter) ContainerStop(ctx context.Context, id string, o dockercontainer.StopOptions) error {
	return a.c.ContainerStop(ctx, id, o)
}
func (a clientAdapter) ContainerRemove(ctx context.Context, id string, o dockercontainer.RemoveOptions) error {
	return a.c.ContainerRemove(ctx, id, o)
}
func (a clientAdapter) ContainerInspect(ctx context.Context, id string) (dockercontainer.InspectResponse, error) {
	return a.c.ContainerInspect(ctx, id)
}
func (a clientAdapter) ImagePull(ctx context.Context, ref string, o dockerimage.PullOptions) (ImagePullReadCloser, error) {
	return a.c.ImagePull(ctx, ref, o)
}
func (a clientAdapter) Events(ctx context.Context, o dockerevents.ListOptions) (<-chan dockerevents.Message, <-chan error) {
	return a.c.Events(ctx, o)
}
func (a clientAdapter) Ping(ctx context.Context) (types.Ping, error) {
	return a.c.Ping(ctx)
}
