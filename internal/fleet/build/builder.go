// Package build wires a concrete fleet.Backend from runtime configuration.
// It lives outside the fleet package itself to avoid an import cycle:
// fleet.Backend is consumed by every backend subpackage (docker, agones,
// plugin), so the factory that imports them all sits one level down.
package build

import (
	"fmt"
	"strings"

	"github.com/ggscale/ggscale/internal/fleet"
	agonesbackend "github.com/ggscale/ggscale/internal/fleet/agones"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
	pluginbackend "github.com/ggscale/ggscale/internal/fleet/plugin"
)

// Config is the runtime input to New. Backend selects which subpackage is
// wired; the Docker*/Agones* fields are only consulted when their backend
// is selected.
type Config struct {
	Backend       string
	Region        string
	PluginDir     string
	GameServerIP  string
	DockerImage   string
	DockerPort    int
	DockerProbe   string
	DockerProbeP  string
	DockerMaxSess int
	DockerHost    string
	AgonesNS      string
	AgonesFleet   string
	AgonesLabels  string
	AgonesKubecfg string
}

// New constructs a fleet.Backend for the configured selector. An empty or
// unrecognised value returns an error; the host wraps that into a startup
// failure rather than silently running with no allocator.
func New(c Config) (fleet.Backend, error) {
	switch c.Backend {
	case "docker":
		if c.DockerImage == "" {
			return nil, fmt.Errorf("fleet: docker backend requires DOCKER_GAMESERVER_IMAGE")
		}
		return dockerbackend.NewFromEnv(dockerbackend.Config{
			Image:     c.DockerImage,
			Port:      c.DockerPort,
			ProbeType: c.DockerProbe,
			ProbePath: c.DockerProbeP,
			PublicIP:  c.GameServerIP,
		})
	case "agones":
		return agonesbackend.NewFromKubeconfig(agonesbackend.Config{
			Namespace:      c.AgonesNS,
			FleetName:      c.AgonesFleet,
			SelectorLabels: agonesbackend.ParseSelectorLabels(c.AgonesLabels),
		}, c.AgonesKubecfg)
	default:
		if name, ok := strings.CutPrefix(c.Backend, "plugin:"); ok {
			if name == "" {
				return nil, fmt.Errorf("fleet: plugin backend requires a name, e.g. plugin:ovh")
			}
			return pluginbackend.Launch(pluginbackend.LaunchConfig{
				Dir:  c.PluginDir,
				Name: name,
			})
		}
		return nil, fmt.Errorf("fleet: unknown backend %q", c.Backend)
	}
}
