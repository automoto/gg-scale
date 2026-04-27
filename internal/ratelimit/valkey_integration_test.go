//go:build integration

package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func startValkey(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "valkey/valkey:8",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(shutdownCtx)
	})

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	require.NoError(t, client.Ping(ctx).Err())
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestValkeyLimiter_allows_requests_within_rate(t *testing.T) {
	client := startValkey(t)
	lim := ratelimit.NewValkeyLimiter(client)

	for i := 0; i < 5; i++ {
		dec, err := lim.Allow(context.Background(), "test:1:v1", 60, 60)
		require.NoError(t, err)
		assert.True(t, dec.Allowed, "request %d should pass — burst is 60", i)
	}
}

func TestValkeyLimiter_rejects_when_burst_exhausted(t *testing.T) {
	client := startValkey(t)
	lim := ratelimit.NewValkeyLimiter(client)

	// Drain the bucket: burst=5 means first 5 succeed, sixth fails.
	for i := 0; i < 5; i++ {
		dec, err := lim.Allow(context.Background(), "test:2:v1", 1, 5)
		require.NoError(t, err)
		require.True(t, dec.Allowed, "fill request %d should pass", i)
	}

	dec, err := lim.Allow(context.Background(), "test:2:v1", 1, 5)
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
	assert.Greater(t, dec.RetryAfter, time.Duration(0))
}

func TestValkeyLimiter_refills_over_time(t *testing.T) {
	client := startValkey(t)
	lim := ratelimit.NewValkeyLimiter(client)

	for i := 0; i < 10; i++ {
		_, err := lim.Allow(context.Background(), "test:3:v1", 100, 10)
		require.NoError(t, err)
	}
	dec, err := lim.Allow(context.Background(), "test:3:v1", 100, 10)
	require.NoError(t, err)
	require.False(t, dec.Allowed)

	// Sleep 200ms — at 100 tokens/s that's 20 tokens worth, far more than 1.
	time.Sleep(200 * time.Millisecond)
	dec, err = lim.Allow(context.Background(), "test:3:v1", 100, 10)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "bucket should have refilled after 200ms")
}

func TestValkeyLimiter_isolates_buckets_by_key(t *testing.T) {
	client := startValkey(t)
	lim := ratelimit.NewValkeyLimiter(client)

	for i := 0; i < 5; i++ {
		dec, err := lim.Allow(context.Background(), "test:tenantA:v1", 1, 5)
		require.NoError(t, err)
		require.True(t, dec.Allowed)
	}
	dec, err := lim.Allow(context.Background(), "test:tenantA:v1", 1, 5)
	require.NoError(t, err)
	require.False(t, dec.Allowed, "A is now exhausted")

	dec, err = lim.Allow(context.Background(), "test:tenantB:v1", 1, 5)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "B is a separate bucket and should pass")
}

// TestRatelimit_fairness_under_load is m1.md task 3.4: tenant A hammers above
// its tier limit and gets 429s; tenant B's requests under load are unaffected.
func TestRatelimit_fairness_under_load_does_not_starve_other_tenants(t *testing.T) {
	client := startValkey(t)
	lim := ratelimit.NewValkeyLimiter(client)

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
		for {
			select {
			case <-ticker.C:
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
