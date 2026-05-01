package olric_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
	cacheolric "github.com/ggscale/ggscale/internal/cache/olric"
	"github.com/ggscale/ggscale/internal/cache/storetest"
)

// TestOlricStore_satisfies_cache_Store_contract starts a single-node embedded
// Olric per RunSuite test and runs the contract suite. Each test gets its own
// node so port and partition state are isolated.
func TestOlricStore_satisfies_cache_Store_contract(t *testing.T) {
	storetest.RunSuite(t, func(t *testing.T) cache.Store {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		store, err := cacheolric.New(ctx, cacheolric.Config{
			BindPort:           freePort(t),
			MemberlistBindPort: freePort(t),
			LogLevel:           "ERROR",
			StartTimeout:       30 * time.Second,
		})
		require.NoError(t, err)

		t.Cleanup(func() {
			shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			_ = store.Close(shutdownCtx)
		})
		return store
	})
}
