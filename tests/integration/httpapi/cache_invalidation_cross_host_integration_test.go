//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/cacheinvalidation"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/tenant"
)

func newInvalidationHost(t *testing.T, c *cluster) (*httptest.Server, *cacheinvalidation.Listener) {
	t.Helper()
	pool := db.NewPool(c.appPool)
	store := memory.New()
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	registry := prometheus.NewRegistry()
	keyCache := tenant.NewKeyCache(tenant.NewSQLLookup(c.appPool), time.Hour, 100)
	overrides := ratelimit.NewCachedOverrideStore(ratelimit.NewDBOverrideStore(pool), time.Hour)
	epochs := httpapi.NewSessionEpochCache(pool, time.Hour, registry)
	listener := cacheinvalidation.New(pool, keyCache, overrides, epochs, registry)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		listener.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test", Pool: pool,
		Lookup: keyCache.Lookup, APIKeyCache: keyCache,
		Limiter: ratelimit.NewCacheLimiter(store), RateLimitOverrides: overrides,
		Signer: signer, SessionEpochValidator: epochs, Cache: store, Registry: registry,
		RBAC: authorizer, Hub: realtime.NewHub(), RealtimeHeartbeat: 100 * time.Millisecond,
	}))
	t.Cleanup(srv.Close)
	require.Eventually(t, listener.Healthy, 5*time.Second, 10*time.Millisecond)
	return srv, listener
}

func TestCacheInvalidation_should_revoke_other_host_caches_within_one_second(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cross-host-cache")
	east, _ := newInvalidationHost(t, c)
	west, _ := newInvalidationHost(t, c)
	token, playerID := anonymousLoginWithID(t, east.URL, "cross-host-cache")

	resp, body := authedReq(t, http.MethodGet, west.URL+"/v1/profile", "cross-host-cache", token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	ticket := realtimeTicket(t, west.URL, "cross-host-cache", token)
	_, err := c.bootstrapPool.Exec(t.Context(),
		`UPDATE project_players SET session_epoch = session_epoch + 1 WHERE id = $1`, playerID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		resp, _ := authedReq(t, http.MethodGet, west.URL+"/v1/profile", "cross-host-cache", token, nil)
		return resp.StatusCode == http.StatusUnauthorized
	}, time.Second, 10*time.Millisecond, "epoch revocation did not reach the other host")
	status := dialWithTicket(t, west.URL, ticket)
	require.Equal(t, http.StatusUnauthorized, status, "WebSocket ticket auth must use the invalidated epoch cache")

	// A fresh player keeps the API-key cache warm after the old token is revoked.
	freshToken, _ := anonymousLoginWithID(t, east.URL, "cross-host-cache")
	resp, body = authedReq(t, http.MethodGet, west.URL+"/v1/profile", "cross-host-cache", freshToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO rate_limit_overrides (tenant_id, kind, rate, burst) VALUES ($1, 'api', 0, 0)`, tenantID)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		resp, _ := authedReq(t, http.MethodGet, west.URL+"/v1/profile", "cross-host-cache", freshToken, nil)
		return resp.StatusCode == http.StatusTooManyRequests
	}, time.Second, 10*time.Millisecond, "override change did not reach the other host")

	_, err = c.bootstrapPool.Exec(t.Context(), `UPDATE api_keys SET revoked_at = now() WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	for name, host := range map[string]*httptest.Server{"east": east, "west": west} {
		require.Eventually(t, func() bool {
			resp, _ := authedReq(t, http.MethodGet, host.URL+"/v1/profile", "cross-host-cache", freshToken, nil)
			return resp.StatusCode == http.StatusForbidden
		}, time.Second, 10*time.Millisecond, "API-key revocation did not reach %s", name)
	}
}
