// Package build wires a concrete cache.Store backend from runtime
// configuration. It lives outside the cache package itself to avoid an
// import cycle: cache.Store is consumed by the memory and olric subpackages,
// so the factory that imports both must sit one level down.
package build

import (
	"context"
	"fmt"
	"time"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/memory"
	cacheolric "github.com/ggscale/ggscale/internal/cache/olric"
)

// Config is the runtime input to New. Backend selects which subpackage is
// wired; the Olric* fields are only consulted when Backend == "olric".
type Config struct {
	Backend             string
	OlricBindAddr       string
	OlricBindPort       int
	OlricMemberlistAddr string
	OlricMemberlistPort int
	OlricPeers          []string
	OlricStartTimeout   time.Duration
}

// New constructs a Store backed by the requested backend. Caller must Close
// the returned Store; for the Olric backend that triggers a clean cluster
// leave.
func New(ctx context.Context, c Config) (cache.Store, error) {
	switch c.Backend {
	case "", "memory":
		return memory.New(), nil
	case "olric":
		store, err := cacheolric.New(ctx, cacheolric.Config{
			BindAddr:           c.OlricBindAddr,
			BindPort:           c.OlricBindPort,
			MemberlistBindAddr: c.OlricMemberlistAddr,
			MemberlistBindPort: c.OlricMemberlistPort,
			Peers:              c.OlricPeers,
			StartTimeout:       c.OlricStartTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("cache: olric: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("cache: unknown backend %q", c.Backend)
	}
}
