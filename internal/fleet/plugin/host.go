package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	hclog "github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/ggscale/ggscale/internal/fleet"
)

// validPluginName constrains FLEET_BACKEND=plugin:<name> so the launcher
// cannot resolve a path outside cfg.PluginDir. Operator-controlled input —
// defence-in-depth, not a request-boundary check.
var validPluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// LaunchConfig resolves to the binary path "<Dir>/ggscale-fleet-<Name>".
type LaunchConfig struct {
	Dir  string   // default /etc/ggscale/plugins
	Name string   // suffix after ggscale-fleet-; matches FLEET_BACKEND=plugin:<name>
	Env  []string // extra env appended to os.Environ() for the subprocess
}

// BinaryPath is the absolute path the host expects for the named plugin.
func (c LaunchConfig) BinaryPath() string {
	dir := c.Dir
	if dir == "" {
		dir = "/etc/ggscale/plugins"
	}
	return filepath.Join(dir, "ggscale-fleet-"+c.Name)
}

// Plugin is the host's handle on a running subprocess. It satisfies
// fleet.Backend (via the embedded gRPC client) and io.Closer (Kill on
// shutdown). The optional Manifest carries metadata from the sidecar TOML
// file if present at launch time.
type Plugin struct {
	fleet.Backend
	client   *goplugin.Client
	manifest *Manifest
}

// Manifest returns the parsed sidecar manifest, or nil if none was present.
func (p *Plugin) Manifest() *Manifest {
	if p == nil {
		return nil
	}
	return p.manifest
}

// Close is idempotent; hashicorp/go-plugin guards double-kill internally.
func (p *Plugin) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	p.client.Kill()
	return nil
}

// Pid is the running subprocess PID, or 0 if the plugin is not (yet) live.
func (p *Plugin) Pid() int {
	if p == nil || p.client == nil {
		return 0
	}
	if rc := p.client.ReattachConfig(); rc != nil {
		return rc.Pid
	}
	return 0
}

// pinger is the gRPC liveness probe; *grpcClient implements it. Kept private
// because it is not part of the fleet.Backend public contract.
type pinger interface {
	Ping(ctx context.Context) error
}

// Ping invokes the plugin's gRPC Ping RPC. Returns nil if the backend does
// not implement the probe (e.g. an in-tree backend wrapped for tests).
func (p *Plugin) Ping(ctx context.Context) error {
	if p == nil || p.Backend == nil {
		return errors.New("fleet plugin: ping on nil plugin")
	}
	if pp, ok := p.Backend.(pinger); ok {
		return pp.Ping(ctx)
	}
	return nil
}

// Launch starts the named plugin binary and returns a Backend wrapper around
// the resulting gRPC client. Caller owns the returned *Plugin and must Close
// it during shutdown.
func Launch(cfg LaunchConfig) (*Plugin, error) {
	if !validPluginName.MatchString(cfg.Name) {
		return nil, errors.New("fleet plugin: invalid plugin name (lowercase alphanumerics, _ and -, max 64 chars)")
	}
	manifest, err := readManifest(cfg.BinaryPath())
	if err != nil {
		return nil, err
	}
	// #nosec G204 -- bin is composed from cfg.Dir (operator config) and a
	// regex-validated plugin name; no untrusted input flows in.
	cmd := exec.Command(cfg.BinaryPath())
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          Plugins(nil),
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		Logger:           hclog.NewNullLogger(),
	})
	rpc, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("fleet plugin %q: connect: %w", cfg.Name, err)
	}
	raw, err := rpc.Dispense(PluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("fleet plugin %q: dispense: %w", cfg.Name, err)
	}
	backend, ok := raw.(fleet.Backend)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("fleet plugin %q: unexpected impl type %T", cfg.Name, raw)
	}
	return &Plugin{Backend: backend, client: client, manifest: manifest}, nil
}

var _ io.Closer = (*Plugin)(nil)
