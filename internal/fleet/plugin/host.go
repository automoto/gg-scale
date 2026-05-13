package plugin

import (
	"errors"
	"fmt"
	"io"
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
	Dir  string // default /etc/ggscale/plugins
	Name string // suffix after ggscale-fleet-; matches FLEET_BACKEND=plugin:<name>
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
// shutdown). Full supervisor — Ping every 10s, restart on crash, manifest
// discovery — lands with the reference plugin in M4.3.
type Plugin struct {
	fleet.Backend
	client *goplugin.Client
}

// Close is idempotent; hashicorp/go-plugin guards double-kill internally.
func (p *Plugin) Close() error {
	if p == nil || p.client == nil {
		return nil
	}
	p.client.Kill()
	return nil
}

// Launch starts the named plugin binary and returns a Backend wrapper around
// the resulting gRPC client. Caller owns the returned *Plugin and must Close
// it during shutdown.
func Launch(cfg LaunchConfig) (*Plugin, error) {
	if !validPluginName.MatchString(cfg.Name) {
		return nil, errors.New("fleet plugin: invalid plugin name (lowercase alphanumerics, _ and -, max 64 chars)")
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: Handshake,
		Plugins:         Plugins(nil),
		// #nosec G204 -- bin is composed from cfg.Dir (operator config) and a
		// regex-validated plugin name; no untrusted input flows in.
		Cmd:              exec.Command(cfg.BinaryPath()),
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
	return &Plugin{Backend: backend, client: client}, nil
}

var _ io.Closer = (*Plugin)(nil)
