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
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/relay"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestFeatureEnabled_disabledGrantOverridesEnabledGrantAfterCacheRefresh(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "feature-cache")
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)

	enabled, err := authorizer.FeatureEnabled(t.Context(), tenantID, projectID, rbac.FeatureP2PRelay)
	require.NoError(t, err)
	assert.True(t, enabled)

	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE feature_grants
		    SET enabled = false, updated_at = now(), reason = 'integration test revoked'
		  WHERE tenant_id = $1 AND project_id = $2 AND feature = $3`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)

	cached, err := authorizer.FeatureEnabled(t.Context(), tenantID, projectID, rbac.FeatureP2PRelay)
	require.NoError(t, err)
	assert.True(t, cached, "feature grants are cached briefly after revocation")

	time.Sleep(6 * time.Second)
	disabled, err := authorizer.FeatureEnabled(t.Context(), tenantID, projectID, rbac.FeatureP2PRelay)
	require.NoError(t, err)
	assert.False(t, disabled, "disabled feature_grants row must deny after cache refresh")
}

func TestRelayCredentials_deniesAfterFeatureGrantRevokedAndCacheRefresh(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "relay-deprovision")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE api_keys
		    SET key_type = 'publishable',
		        scopes = ARRAY['p2p_relay']::text[]
		  WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)

	srv := newRelayServerForCluster(t, c)
	access := anonymousLogin(t, srv.URL, "relay-deprovision")
	issue := func() int {
		resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/relay/credentials", "relay-deprovision", access, nil)
		assert.NotContains(t, string(body), "panic")
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusOK, issue())

	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE feature_grants
		    SET enabled = false, updated_at = now(), reason = 'integration test revoked'
		  WHERE tenant_id = $1 AND project_id = $2 AND feature = $3`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, issue(), "feature grants are cached briefly after revocation")
	time.Sleep(6 * time.Second)
	assert.Equal(t, http.StatusForbidden, issue())
}

func TestFleetAllocationTicket_deniesAfterFeatureGrantRevokedAndCacheRefresh(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-deprovision")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE api_keys
		    SET key_type = 'publishable',
		        scopes = ARRAY['matchmaker', 'fleet']::text[]
		  WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureDedicatedServers))
	require.NoError(t, err)

	backend := newStubBackend("stub")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())
	srv := newFleetMatchmakerServerForCluster(t, c, backend)
	createTicket := func() int {
		access := anonymousLogin(t, srv.URL, "fleet-deprovision")
		resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/matchmaker/tickets", "fleet-deprovision", access, map[string]any{
			"mode":  "fleet_allocation",
			"fleet": fleetName,
		})
		assert.NotContains(t, string(body), "panic")
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusCreated, createTicket())

	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE feature_grants
		    SET enabled = false, updated_at = now(), reason = 'integration test revoked'
		  WHERE tenant_id = $1 AND project_id = $2 AND feature = $3`,
		tenantID, projectID, string(rbac.FeatureDedicatedServers))
	require.NoError(t, err)

	assert.Equal(t, http.StatusCreated, createTicket(), "feature grants are cached briefly after revocation")
	time.Sleep(6 * time.Second)
	assert.Equal(t, http.StatusForbidden, createTicket())
}

func TestMatchmakerTicket_deniesAfterExplicitDisableGrantAndCacheRefresh(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "mm-deprovision")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE api_keys
		    SET key_type = 'publishable',
		        scopes = ARRAY['matchmaker']::text[]
		  WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)

	srv := newFleetMatchmakerServerForCluster(t, c, newStubBackend("stub"))
	createTicket := func() int {
		access := anonymousLogin(t, srv.URL, "mm-deprovision")
		resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/matchmaker/tickets", "mm-deprovision", access, map[string]any{
			"mode":      "match_only",
			"game_mode": "ffa",
			"min_count": 1,
			"max_count": 2,
		})
		assert.NotContains(t, string(body), "panic")
		return resp.StatusCode
	}

	// Matchmaker is default-on: with no grant row the ticket is accepted.
	assert.Equal(t, http.StatusCreated, createTicket())

	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, false, 'integration test deprovision')`,
		tenantID, projectID, string(rbac.FeatureMatchmaker))
	require.NoError(t, err, "an explicit enabled=false matchmaker row must be storable")

	time.Sleep(6 * time.Second)
	assert.Equal(t, http.StatusForbidden, createTicket())
}

func newRelayServerForCluster(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	h := httpapi.NewRouter(httpapi.Deps{
		Version:     "v1",
		Commit:      "test",
		Pool:        pool,
		Lookup:      tenant.NewSQLLookup(c.appPool),
		Limiter:     ratelimit.NewCacheLimiter(c.cache),
		Signer:      signer,
		Cache:       c.cache,
		RBAC:        authorizer,
		RelayIssuer: relay.NewIssuer("shared-relay-secret", "relay.test", time.Minute),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func newFleetMatchmakerServerForCluster(t *testing.T, c *cluster, backend fleet.Backend) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	mgr := fleet.NewManager(
		fleet.NewPostgresStore(pool),
		fleet.NewPostgresFleetStore(pool),
		backend,
		fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }},
	)

	h := httpapi.NewRouter(httpapi.Deps{
		Version:    "v1",
		Commit:     "test",
		Pool:       pool,
		Lookup:     tenant.NewSQLLookup(c.appPool),
		Limiter:    ratelimit.NewCacheLimiter(c.cache),
		Signer:     signer,
		Cache:      c.cache,
		RBAC:       authorizer,
		Fleet:      mgr,
		Matchmaker: matchmaker.NewMemQueue(),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
