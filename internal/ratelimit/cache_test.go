package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

// -------- CacheLimiter --------

func TestCacheLimiter_allows_requests_within_burst(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		dec, err := lim.Allow(ctx, "user:1:v1", 60, 60)
		require.NoError(t, err)
		assert.True(t, dec.Allowed, "request %d should pass — burst is 60", i)
	}
}

func TestCacheLimiter_rejects_when_burst_exhausted(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := lim.Allow(ctx, "user:2:v1", 1, 5)
		require.NoError(t, err)
	}

	dec, err := lim.Allow(ctx, "user:2:v1", 1, 5)
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
	assert.Greater(t, dec.RetryAfter, time.Duration(0))
}

func TestCacheLimiter_isolates_buckets_by_key(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := lim.Allow(ctx, "user:A:v1", 1, 5)
		require.NoError(t, err)
	}
	dec, err := lim.Allow(ctx, "user:A:v1", 1, 5)
	require.NoError(t, err)
	require.False(t, dec.Allowed, "A is exhausted")

	dec, err = lim.Allow(ctx, "user:B:v1", 1, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "B is independent of A")
}

// TestRatelimit_fairness_under_load is m1.md task 3.4: tenant A hammers above
// its tier limit and gets 429s; tenant B's modest traffic under load is
// unaffected. With a per-process Store this runs without any container.
func TestRatelimit_fairness_under_load_does_not_starve_other_tenants(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	keyA := tenant.APIKey{ID: 1001, TenantID: 1, Tier: tenant.TierFree}
	keyB := tenant.APIKey{ID: 2002, TenantID: 2, Tier: tenant.TierFree}

	var aOK, a429, bOK, bDenied atomic.Int64
	deadline := time.Now().Add(800 * time.Millisecond)

	hitA := func() {
		for time.Now().Before(deadline) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			req = req.WithContext(tenant.WithAPIKey(req.Context(), keyA))
			handler.ServeHTTP(rr, req)
			switch rr.Code {
			case http.StatusOK:
				aOK.Add(1)
			case http.StatusTooManyRequests:
				a429.Add(1)
			}
		}
	}
	hitB := func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if !time.Now().Before(deadline) {
				return
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			req = req.WithContext(tenant.WithAPIKey(req.Context(), keyB))
			handler.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				bOK.Add(1)
			} else {
				bDenied.Add(1)
			}
		}
	}

	done := make(chan struct{}, 9)
	for i := 0; i < 8; i++ {
		go func() { hitA(); done <- struct{}{} }()
	}
	go func() { hitB(); done <- struct{}{} }()
	for i := 0; i < 9; i++ {
		<-done
	}

	t.Logf("tenant A: %d ok, %d 429", aOK.Load(), a429.Load())
	t.Logf("tenant B: %d ok, %d denied", bOK.Load(), bDenied.Load())

	assert.Greater(t, a429.Load(), int64(0), "tenant A must be throttled")
	assert.Greater(t, bOK.Load(), int64(20), "tenant B should still get most of its modest traffic through")
	assert.Equal(t, int64(0), bDenied.Load(), "tenant B's polite traffic must not be throttled by A's hammering")
}

// -------- CacheConnectionCap --------

func TestCacheConnectionCap_acquire_within_limit_succeeds(t *testing.T) {
	cap := ratelimit.NewCacheConnectionCap(memory.New())
	dec, err := cap.Acquire(context.Background(), ratelimit.ConnectionCapKey(1001), 5)

	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, int64(1), dec.Current)
}

func TestCacheConnectionCap_acquire_at_limit_rejects_and_rolls_back(t *testing.T) {
	cap := ratelimit.NewCacheConnectionCap(memory.New())
	key := ratelimit.ConnectionCapKey(1002)

	for i := 0; i < 5; i++ {
		dec, err := cap.Acquire(context.Background(), key, 5)
		require.NoError(t, err)
		require.True(t, dec.Allowed)
	}

	dec, err := cap.Acquire(context.Background(), key, 5)
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
	assert.Equal(t, int64(5), dec.Current, "counter must roll back to limit, not 6")
}

func TestCacheConnectionCap_release_frees_a_slot(t *testing.T) {
	cap := ratelimit.NewCacheConnectionCap(memory.New())
	key := ratelimit.ConnectionCapKey(1003)

	for i := 0; i < 3; i++ {
		_, err := cap.Acquire(context.Background(), key, 3)
		require.NoError(t, err)
	}
	dec, err := cap.Acquire(context.Background(), key, 3)
	require.NoError(t, err)
	require.False(t, dec.Allowed, "fourth connection over limit is rejected")

	require.NoError(t, cap.Release(context.Background(), key))

	dec, err = cap.Acquire(context.Background(), key, 3)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "after release a new connection fits")
	assert.Equal(t, int64(3), dec.Current)
}

func TestCacheConnectionCap_isolates_counts_by_key(t *testing.T) {
	cap := ratelimit.NewCacheConnectionCap(memory.New())
	keyA := ratelimit.ConnectionCapKey(2001)
	keyB := ratelimit.ConnectionCapKey(2002)

	for i := 0; i < 5; i++ {
		_, err := cap.Acquire(context.Background(), keyA, 5)
		require.NoError(t, err)
	}
	dec, err := cap.Acquire(context.Background(), keyA, 5)
	require.NoError(t, err)
	require.False(t, dec.Allowed)

	dec, err = cap.Acquire(context.Background(), keyB, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "tenant B's counter is independent of A's")
	assert.Equal(t, int64(1), dec.Current)
}

func TestCacheConnectionCap_refresh_keeps_counter_alive_past_idle_ttl(t *testing.T) {
	store := memory.New()
	cap := ratelimit.NewCacheConnectionCap(store)
	key := ratelimit.ConnectionCapKey(3001)

	// Acquire one slot — its idle TTL is 6h by default, so we cannot exercise
	// expiry without manipulating the store directly. Instead, verify that
	// Refresh against the live counter is a no-op error-free path.
	_, err := cap.Acquire(context.Background(), key, 5)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoError(t, cap.Refresh(context.Background(), key))
	}

	dec, err := cap.Acquire(context.Background(), key, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, int64(2), dec.Current, "counter survived the refresh sequence")
}

func TestCacheConnectionCap_release_clamps_at_zero(t *testing.T) {
	cap := ratelimit.NewCacheConnectionCap(memory.New())
	key := ratelimit.ConnectionCapKey(4001)

	require.NoError(t, cap.Release(context.Background(), key))
	require.NoError(t, cap.Release(context.Background(), key))

	dec, err := cap.Acquire(context.Background(), key, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, int64(1), dec.Current, "double-release on empty must not push the counter negative")
}
