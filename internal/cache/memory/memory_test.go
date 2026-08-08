package memory_test

import (
	"testing"

	"github.com/automoto/gg-scale/internal/cache"
	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/cache/storetest"
)

func TestMemoryStore_satisfies_cache_Store_contract(t *testing.T) {
	storetest.RunSuite(t, func(_ *testing.T) cache.Store {
		return memory.New()
	})
}
