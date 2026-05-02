// Package build wires a concrete cache.Store backend from runtime
// configuration. It lives outside the cache package itself to avoid an
// import cycle: cache.Store is consumed by the memory and olric subpackages,
// so the factory that imports both must sit one level down.
package build

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/instrument"
	"github.com/ggscale/ggscale/internal/cache/memory"
	cacheolric "github.com/ggscale/ggscale/internal/cache/olric"
)

// Config is the runtime input to New. Backend selects which subpackage is
// wired; the Olric* fields are only consulted when Backend == "olric".
// Registry, if non-nil, receives ggscale_cache_ops_total counters via an
// instrumentation wrapper applied after the backend is constructed.
type Config struct {
	Backend             string
	OlricBindAddr       string
	OlricBindPort       int
	OlricMemberlistAddr string
	OlricMemberlistPort int
	OlricPeers          []string
	OlricStartTimeout   time.Duration
	Registry            prometheus.Registerer
}

// New constructs a Store backed by the requested backend. Caller must Close
// the returned Store; for the Olric backend that triggers a clean cluster
// leave.
func New(ctx context.Context, c Config) (cache.Store, error) {
	var store cache.Store
	switch c.Backend {
	case "", "memory":
		store = memory.New()
	case "olric":
		s, err := cacheolric.New(ctx, cacheolric.Config{
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
		store = s
	default:
		return nil, fmt.Errorf("cache: unknown backend %q", c.Backend)
	}
	if c.Registry != nil {
		store = instrument.New(store, c.Registry)
	}
	return store, nil
}
