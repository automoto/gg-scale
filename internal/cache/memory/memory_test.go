package memory_test

import (
	"testing"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/cache/storetest"
)

func TestMemoryStore_satisfies_cache_Store_contract(t *testing.T) {
	storetest.RunSuite(t, func(_ *testing.T) cache.Store {
		return memory.New()
	})
}
