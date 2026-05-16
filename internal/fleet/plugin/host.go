package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	hclog "github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/webutil"
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
	binPath := cfg.BinaryPath()
	manifest, err := readManifest(binPath)
	if err != nil {
		return nil, err
	}
	if err := verifyManifestSHA256(binPath, manifest); err != nil {
		return nil, err
	}
	// #nosec G204 -- bin is composed from cfg.Dir (operator config) and a
	// regex-validated plugin name; no untrusted input flows in.
	cmd := exec.Command(binPath)
	// Strip host secrets (DB credentials, JWT keys, SMTP passwords, …)
	// from the child env. The plugin only sees the safe-allowlist host
	// vars plus whatever the operator explicitly supplied in cfg.Env.
	cmd.Env = webutil.ScrubEnv(cfg.Env)
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

// verifyManifestSHA256 enforces the manifest's optional binary hash. A
// missing sha256 field logs a warning (warn-only mode) and proceeds; a
// declared-but-mismatching hash refuses the launch entirely.
func verifyManifestSHA256(binPath string, m *Manifest) error {
	if m == nil || m.SHA256 == "" {
		slog.Warn("fleet plugin: launching without manifest sha256 verification",
			"binary", binPath,
			"hint", "add `sha256 = \"<hex>\"` to the .manifest.toml to enable integrity checks")
		return nil
	}
	f, err := os.Open(binPath) // #nosec G304 -- operator-controlled path
	if err != nil {
		return fmt.Errorf("fleet plugin: open binary for sha256: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("fleet plugin: hash binary: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != m.SHA256 {
		return fmt.Errorf("fleet plugin: binary sha256 mismatch (manifest=%s actual=%s)", m.SHA256, actual)
	}
	return nil
}
