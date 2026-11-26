//go:build integration

package db_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cacheinvalidation"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestCacheInvalidation_should_evict_api_key_after_postgres_notify(t *testing.T) {
	// Arrange
	rawPool := startMigrated(t)
	pool := db.NewPool(rawPool)
	var lookups atomic.Int64
	keys := tenant.NewKeyCache(func(context.Context, []byte) (*tenant.APIKey, error) {
		lookups.Add(1)
		return &tenant.APIKey{ID: 9, TenantID: 42}, nil
	}, time.Hour, 10)
	sender := tenant.NewKeyCache(nil, time.Hour, 10).WithNotifier(pool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cacheinvalidation.Run(ctx, pool, cacheinvalidation.Handler{APIKeys: keys.Invalidate}, nil)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool {
		var ready bool
		err := rawPool.QueryRow(context.Background(), `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND query = 'LISTEN "ggscale_cache_invalidation"'
		)`).Scan(&ready)
		return err == nil && ready
	}, 5*time.Second, 20*time.Millisecond, "cache invalidation LISTEN subscription did not become ready")
	_, err := keys.Lookup(context.Background(), []byte("key"))
	require.NoError(t, err)
	_, err = keys.Lookup(context.Background(), []byte("key"))
	require.NoError(t, err)
	require.Equal(t, int64(1), lookups.Load(), "the second read must be cached")

	// Act: a different instance's write publishes the invalidation. The cache
	// above is not locally evicted by this call.
	sender.InvalidateAndNotify(ctx, 42)

	// Assert
	require.Eventually(t, func() bool {
		_, lookupErr := keys.Lookup(context.Background(), []byte("key"))
		return lookupErr == nil && lookups.Load() == 2
	}, 2*time.Second, 20*time.Millisecond, "NOTIFY did not invalidate the cached key")
	assert.Equal(t, int64(2), lookups.Load())
}
