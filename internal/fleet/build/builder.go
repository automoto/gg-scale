// Package build wires a concrete fleet.Backend from runtime configuration.
// It lives outside the fleet package itself to avoid an import cycle:
// fleet.Backend is consumed by every backend subpackage (docker, agones,
// plugin), so the factory that imports them all sits one level down.
//
// Per-template values (Docker image, Agones Fleet name, plugin opaque
// config) come from fleet templates stored in Postgres, NOT from env vars.
// What this builder configures is host-level: which backend to use, plus
// the credentials/sockets/kubeconfig the backend needs to talk to its
// daemon or API server.
package build

import (
	"fmt"
	"strings"

	"github.com/ggscale/ggscale/internal/fleet"
	agonesbackend "github.com/ggscale/ggscale/internal/fleet/agones"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
	pluginbackend "github.com/ggscale/ggscale/internal/fleet/plugin"
)

// Config is the runtime input to New.
type Config struct {
	Backend       string
	Region        string
	PluginDir     string
	GameServerIP  string
	DockerHost    string
	AgonesNS      string
	AgonesKubecfg string

	// Out-of-cluster k3s auth (Dokku deploys). When all three are set the
	// agones backend builds rest.Config from these instead of AgonesKubecfg.
	// K3sCACertB64 is base64-encoded PEM.
	K3sAPIURL    string
	K3sSAToken   string
	K3sCACertB64 string

	// Docker host-wide knobs surfaced from internal/config.
	DockerBindIP            string
	DockerDefaultMemory     int64
	DockerDefaultNanoCPUs   int64
	DockerDefaultPids       int64
	DockerRegistryAllowlist []string
	DockerRequireDigest     bool
}

// New constructs a fleet.Backend for the configured selector. An empty or
// unrecognised value returns an error; the host wraps that into a startup
// failure rather than silently running with no allocator.
func New(c Config) (fleet.Backend, error) {
	switch c.Backend {
	case "docker":
		return dockerbackend.NewFromEnv(dockerbackend.Config{
			PublicIP:           c.GameServerIP,
			BindIP:             c.DockerBindIP,
			DefaultMemoryBytes: c.DockerDefaultMemory,
			DefaultNanoCPUs:    c.DockerDefaultNanoCPUs,
			DefaultPidsLimit:   c.DockerDefaultPids,
			RegistryAllowlist:  c.DockerRegistryAllowlist,
			RequireDigest:      c.DockerRequireDigest,
		})
	case "agones":
		agonesCfg := agonesbackend.Config{Namespace: c.AgonesNS}
		if c.K3sAPIURL != "" {
			return agonesbackend.NewFromEnvVars(agonesCfg, c.K3sAPIURL, c.K3sSAToken, c.K3sCACertB64)
		}
		return agonesbackend.NewFromKubeconfig(agonesCfg, c.AgonesKubecfg)
	default:
		if name, ok := strings.CutPrefix(c.Backend, "plugin:"); ok {
			if name == "" {
				return nil, fmt.Errorf("fleet: plugin backend requires a name, e.g. plugin:ovh")
			}
			return pluginbackend.NewSupervisor(pluginbackend.SupervisorConfig{
				Launch: pluginbackend.LaunchConfig{Dir: c.PluginDir, Name: name},
			})
		}
		return nil, fmt.Errorf("fleet: unknown backend %q", c.Backend)
	}
}
