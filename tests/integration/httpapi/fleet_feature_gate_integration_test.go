//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/serverlist"
	"github.com/ggscale/ggscale/internal/tenant"
)

func newServerListServer(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	h := httpapi.NewRouter(httpapi.Deps{
		Version:    "v1",
		Commit:     "test",
		Pool:       pool,
		Lookup:     tenant.NewSQLLookup(c.appPool),
		Limiter:    ratelimit.NewCacheLimiter(c.cache),
		Signer:     signer,
		Cache:      c.cache,
		RBAC:       authorizer,
		ServerList: serverlist.New(time.Minute),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// seedFleetScopedKey flips the tenant's secret key to carry only the fleet
// scope — the server-tier heartbeat auth surface.
func seedFleetScopedKey(t *testing.T, c *cluster, tenantID, projectID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE api_keys SET scopes = ARRAY['fleet']::text[] WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)
}

func fleetHeartbeatBody() map[string]any {
	return map[string]any{
		"agones_name": "gs-1",
		"fleet":       "default",
		"address":     "203.0.113.10:7777",
		"max_players": 16,
	}
}

func TestFleetHeartbeat_deniedWithoutDedicatedServersEntitlement(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-hb-deny")
	seedFleetScopedKey(t, c, tenantID, projectID)
	srv := newServerListServer(t, c)

	// No dedicated_servers grant: the fleet scope alone must not admit a
	// heartbeat once the entitlement is absent.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/fleets/heartbeat", "fleet-hb-deny", fleetHeartbeatBody())
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestFleetHeartbeat_allowedWithDedicatedServersEntitlement(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-hb-allow")
	seedFleetScopedKey(t, c, tenantID, projectID)
	// Grant present before the first call, so the feature cache loads enabled.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureDedicatedServers))
	require.NoError(t, err)
	srv := newServerListServer(t, c)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/fleets/heartbeat", "fleet-hb-allow", fleetHeartbeatBody())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
}
