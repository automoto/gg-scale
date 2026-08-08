//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

// authIPBurst is the fixed per-IP auth bucket size. The token-route tests
// send more than this from one IP to prove that bucket no longer binds;
// they stay below the tier_0 token-route per-IP burst (50) and far below
// the tier_0 per-key bucket (250/s, burst 500).
const authIPBurst = int(ratelimit.AuthIPBurst)

// tinyTokenIPLimits makes the token-route per-IP limiter trip
// deterministically: burst 3, effectively no refill.
func tinyTokenIPLimits(tenant.Tier) ratelimit.Limits {
	return ratelimit.Limits{RatePerSecond: 0.001, Burst: 3}
}

// seedTenantWithPublishableKey mirrors seedTenantWithAPIKey but marks the
// key publishable (the type embedded in shipped game clients); the plain
// helper inherits the schema default, key_type='secret'.
func seedTenantWithPublishableKey(t *testing.T, pool *pgxpool.Pool, tier int16, token string) {
	t.Helper()
	ctx := context.Background()
	var tenantID, projectID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name, tier) VALUES ($1, $2) RETURNING id`,
		"tenant-"+token, tier).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
		tenantID, "project-"+token).Scan(&projectID))
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(ctx,
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type) VALUES ($1, $2, $3, 'publishable')`,
		tenantID, projectID, sum[:])
	require.NoError(t, err)
}

func TestAuthAnonymous_should_not_throttle_above_ip_burst(t *testing.T) {
	c := startCluster(t)
	seedTenantWithPublishableKey(t, c.bootstrapPool, 0, "anon-burst")
	srv := newServerForCluster(t, c)

	for i := range authIPBurst + 10 {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "anon-burst", nil)
		require.Equalf(t, http.StatusOK, resp.StatusCode, "request %d: %s", i+1, string(body))
	}
}

func TestAuthTokenRoutes_should_not_throttle_above_ip_burst(t *testing.T) {
	routes := []string{
		"/v1/auth/refresh",
		"/v1/auth/verify",
		"/v1/auth/logout",
		"/v1/auth/custom-token",
	}
	c := startCluster(t)
	seedTenantWithPublishableKey(t, c.bootstrapPool, 0, "token-burst")
	srv := newServerForCluster(t, c)

	// authIPBurst+1 per route so even a single route wrongly remounted
	// under the fixed per-IP limiter overflows its bucket and fails here.
	for _, route := range routes {
		for i := range authIPBurst + 1 {
			resp, body := doJSON(t, http.MethodPost, srv.URL+route, "token-burst", map[string]string{})
			assert.NotEqualf(t, http.StatusTooManyRequests, resp.StatusCode,
				"%s request %d: %s", route, i+1, string(body))
		}
	}
}

func TestAuthTokenRoutes_should_throttle_publishable_keys_at_per_ip_limit(t *testing.T) {
	c := startCluster(t)
	seedTenantWithPublishableKey(t, c.bootstrapPool, 0, "tiny-pub")
	srv := newServerWithTokenIPLimits(t, c, tinyTokenIPLimits)

	for i := range 3 {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "tiny-pub", nil)
		require.Equalf(t, http.StatusOK, resp.StatusCode, "request %d: %s", i+1, string(body))
	}

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "tiny-pub", nil)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
}

func TestAuthTokenRoutes_should_exempt_secret_keys_from_per_ip_limit(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "tiny-secret")
	srv := newServerWithTokenIPLimits(t, c, tinyTokenIPLimits)

	for i := range 10 {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "tiny-secret", nil)
		require.Equalf(t, http.StatusOK, resp.StatusCode, "request %d: %s", i+1, string(body))
	}
}

func TestAuthPasswordRoutes_should_throttle_above_ip_burst(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "password-burst")
	srv := newServerForCluster(t, c)

	// Drain the shared per-IP bucket with malformed logins; validation
	// rejects them before any bcrypt work, but each consumes a token.
	for i := range authIPBurst {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "password-burst", map[string]string{})
		require.NotEqualf(t, http.StatusTooManyRequests, resp.StatusCode, "request %d: %s", i+1, string(body))
	}

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "password-burst", map[string]string{})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "password-burst", map[string]string{})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
}
