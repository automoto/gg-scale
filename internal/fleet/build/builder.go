// Package build wires a concrete fleet.Backend from runtime configuration.
// It lives outside the fleet package itself to avoid an import cycle:
// fleet.Backend is consumed by every backend subpackage (docker, agones,
// openstack, plugin), so the factory that imports them all sits one level
// down.
package build

import (
	"fmt"
	"strings"

	"github.com/ggscale/ggscale/internal/fleet"
	dockerbackend "github.com/ggscale/ggscale/internal/fleet/docker"
)

// Config is the runtime input to New. Backend selects which subpackage is
// wired; the Docker* fields are only consulted when Backend == "docker".
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
	case "agones", "openstack":
		return nil, fmt.Errorf("fleet: backend %q is planned for a later milestone", c.Backend)
	default:
		if strings.HasPrefix(c.Backend, "plugin:") {
			return nil, fmt.Errorf("fleet: plugin backends ship in M4")
		}
		return nil, fmt.Errorf("fleet: unknown backend %q", c.Backend)
	}
}
