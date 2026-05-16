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
	"regexp"
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

// Config wires the backend at startup. Per-template values (image, port,
// probe) come in through AllocationRequest.Config on each Allocate; what
// remains here are host-level knobs that are deployment-wide.
type Config struct {
	// Client is the Docker daemon adapter. Pass *client.Client in production.
	Client API
	// ProbeTimeout bounds how long Allocate waits for the probe to succeed.
	// Default 30s when zero.
	ProbeTimeout time.Duration
	// PublicIP is the host or IP returned to game clients. Required when
	// the daemon is not on localhost or when the published port isn't
	// reachable on 127.0.0.1.
	PublicIP string
	// BindIP is the host interface published container ports bind to.
	// Default "127.0.0.1". Use the public IP for production multi-host.
	BindIP string
	// DefaultMemoryBytes / DefaultNanoCPUs / DefaultPidsLimit are the
	// per-container resource caps applied when a fleet template does not
	// specify its own. Zero leaves the daemon default (unbounded).
	DefaultMemoryBytes int64
	DefaultNanoCPUs    int64
	DefaultPidsLimit   int64
	// RegistryAllowlist restricts which registries may run. Empty disables
	// the check; non-empty rejects images whose canonical reference does
	// not start with one of the listed prefixes.
	RegistryAllowlist []string
	// RequireDigest enforces that every image carries an @sha256:… pin.
	// Recommended on for production deployments.
	RequireDigest bool
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
	if cfg.BindIP == "" {
		cfg.BindIP = "127.0.0.1"
	}
	// PublicIP-pointing-at-loopback is a config smell rather than a hard
	// error: legitimate dev/CI setups bind both PublicIP and BindIP to
	// 127.0.0.1. config.Validate flags this in production.
	timeout := cfg.ProbeTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Backend{cfg: cfg, probeTimeout: timeout}, nil
}

// validProbePath restricts the operator-supplied path on each template to
// printable URL-path characters. Prevents a malformed value from
// constructing a probe URL that escapes the container address.
var validProbePath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@/%-]*$`)

// Name returns the backend identifier persisted on every allocation row.
func (b *Backend) Name() string { return "docker" }

// Template captures the per-allocation knobs the docker backend reads off
// AllocationRequest.Config. Operators set these by creating a fleet in the
// dashboard; the manager flattens fleet.config into req.Config before
// dispatching.
type Template struct {
	Image     string
	Port      int
	ProbeType string
	ProbePath string
	PullImage bool
}

// TemplateFromConfig parses AllocationRequest.Config keys into a Template
// and validates the required fields (image, port). Returns a meaningful
// error if either is missing or malformed so the dashboard can surface it
// against the fleet row.
func (b *Backend) TemplateFromConfig(cfg map[string]string) (Template, error) {
	t := Template{
		Image:     cfg["image"],
		ProbeType: cfg["probe_type"],
		ProbePath: cfg["probe_path"],
	}
	if t.Image == "" {
		return Template{}, errors.New("docker: fleet config missing \"image\"")
	}
	if err := b.validateImage(t.Image); err != nil {
		return Template{}, err
	}
	if t.ProbePath != "" && !validProbePath.MatchString(t.ProbePath) {
		return Template{}, fmt.Errorf("docker: fleet config invalid \"probe_path\" %q", t.ProbePath)
	}
	if raw := cfg["port"]; raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 {
			return Template{}, fmt.Errorf("docker: fleet config invalid \"port\" %q", raw)
		}
		t.Port = port
	} else {
		return Template{}, errors.New("docker: fleet config missing \"port\"")
	}
	t.PullImage = cfg["pull_image"] == "true"
	return t, nil
}

// TemplateFromConfig is the package-level shim used by callers that don't
// have a backend instance handy (notably the dashboard fleet form, which
// validates templates before persisting). Registry policy isn't checked
// without a backend; the request-time path still does.
func TemplateFromConfig(cfg map[string]string) (Template, error) {
	return (&Backend{}).TemplateFromConfig(cfg)
}

func (b *Backend) validateImage(image string) error {
	if b.cfg.RequireDigest && !strings.Contains(image, "@sha256:") {
		return fmt.Errorf("docker: image %q lacks digest pin (require_digest is set)", image)
	}
	if len(b.cfg.RegistryAllowlist) == 0 {
		return nil
	}
	for _, prefix := range b.cfg.RegistryAllowlist {
		if strings.HasPrefix(image, prefix) {
			return nil
		}
	}
	return fmt.Errorf("docker: image %q not in registry allowlist", image)
}

// Allocate creates, starts, and probes a container. On any failure past
// ContainerCreate the container is force-removed so a failed Allocate
// leaves no orphan resources behind.
func (b *Backend) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	tmpl, err := b.TemplateFromConfig(req.Config)
	if err != nil {
		return nil, err
	}

	if tmpl.PullImage {
		if err := b.pullImage(ctx, tmpl.Image); err != nil {
			return nil, err
		}
	}

	containerPort, err := nat.NewPort("tcp", strconv.Itoa(tmpl.Port))
	if err != nil {
		return nil, fmt.Errorf("docker: port: %w", err)
	}

	pidsLimit := b.cfg.DefaultPidsLimit
	var pidsPtr *int64
	if pidsLimit > 0 {
		pidsPtr = &pidsLimit
	}
	created, err := b.cfg.Client.ContainerCreate(ctx,
		&dockercontainer.Config{
			Image:        tmpl.Image,
			ExposedPorts: nat.PortSet{containerPort: struct{}{}},
			Labels: map[string]string{
				"ggscale.managed_by": "ggscale.fleet",
				"ggscale.tenant_id":  strconv.FormatInt(req.TenantID, 10),
				"ggscale.project_id": strconv.FormatInt(req.ProjectID, 10),
				"ggscale.fleet_id":   strconv.FormatInt(req.FleetID, 10),
				"ggscale.region":     req.Region,
				"ggscale.game_mode":  req.GameMode,
			},
		},
		&dockercontainer.HostConfig{
			PortBindings: nat.PortMap{
				containerPort: []nat.PortBinding{{HostIP: b.cfg.BindIP, HostPort: "0"}},
			},
			AutoRemove: false,
			// Safe defaults for a multi-tenant platform: game servers
			// never need elevated privileges (we publish to a dynamic
			// high port, no CAP_NET_BIND_SERVICE required). Operators
			// who need to relax this should add a config knob rather
			// than weaken the default.
			SecurityOpt:    []string{"no-new-privileges:true"},
			CapDrop:        []string{"ALL"},
			ReadonlyRootfs: true,
			Resources: dockercontainer.Resources{
				Memory:    b.cfg.DefaultMemoryBytes,
				NanoCPUs:  b.cfg.DefaultNanoCPUs,
				PidsLimit: pidsPtr,
			},
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

	if err := b.probe(ctx, address, tmpl); err != nil {
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

func (b *Backend) pullImage(ctx context.Context, image string) error {
	rc, err := b.cfg.Client.ImagePull(ctx, image, dockerimage.PullOptions{})
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

func (b *Backend) probe(ctx context.Context, address string, tmpl Template) error {
	if tmpl.ProbeType == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()

	deadline := time.NewTimer(b.probeTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if b.probeOnce(probeCtx, address, tmpl) {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("docker: probe %s did not succeed within %s", address, b.probeTimeout)
		case <-ticker.C:
		}
	}
}

func (b *Backend) probeOnce(ctx context.Context, address string, tmpl Template) bool {
	switch strings.ToLower(tmpl.ProbeType) {
	case "tcp":
		d := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	case "http":
		path := tmpl.ProbePath
		if path == "" {
			path = "/"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			return false
		}
		// Block redirects: a malicious image could 302 the probe at the
		// cloud metadata service or an internal endpoint (blind SSRF).
		// The probe should only validate that the container itself
		// responds — anything else fails closed.
		c := http.Client{
			Timeout:       500 * time.Millisecond,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		// Treat redirects as probe failures: the container is delegating
		// responsibility elsewhere, which is exactly what we don't want.
		return resp.StatusCode >= 200 && resp.StatusCode < 300
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
