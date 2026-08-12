//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

// newFleetPGQueueServer mirrors newFleetMatchmakerServerForCluster but backs
// the matchmaker with the production Postgres queue, so the per-player
// allocation cap (a PGQueue enqueue guard that counts matchmaker_matches) is
// exercised end to end.
func newFleetPGQueueServer(t *testing.T, c *cluster, backend fleet.Backend) *httptest.Server {
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
		Matchmaker: matchmaker.NewPGQueue(pool),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestFleetAllocationTicket_cappedPerPlayerUnclaimed proves the per-player cap
// on concurrent unclaimed fleet allocations: a player already holding the cap's
// worth of unclaimed dedicated-server allocations is refused a new fleet ticket
// (429), and freeing a slot lets the next request through. This bounds the
// enqueue -> matched -> abandon loop that would otherwise hoard fleet servers
// until the 24h match GC.
func TestFleetAllocationTicket_cappedPerPlayerUnclaimed(t *testing.T) {
	ctx := context.Background()
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-cap")
	_, err := c.bootstrapPool.Exec(ctx,
		`UPDATE api_keys
		    SET key_type = 'publishable',
		        scopes = ARRAY['matchmaker', 'fleet']::text[]
		  WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(ctx,
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureDedicatedServers))
	require.NoError(t, err)

	backend := newStubBackend("stub")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())
	var fleetID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT id FROM fleets WHERE tenant_id = $1 AND project_id = $2 AND name = $3`,
		tenantID, projectID, fleetName).Scan(&fleetID))
	srv := newFleetPGQueueServer(t, c, backend)

	access, playerID := anonymousLoginWithID(t, srv.URL, "fleet-cap")
	createTicket := func() int {
		resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/matchmaker/tickets", "fleet-cap", access, map[string]any{
			"mode":  "fleet_allocation",
			"fleet": fleetName,
		})
		assert.NotContains(t, string(body), "panic")
		return resp.StatusCode
	}

	// Seed the default cap (3) of unclaimed, unexpired fleet allocations the
	// player already holds — as if three matched tickets were never claimed.
	// Fleet matches carry no host, so the player is recorded in the roster.
	// matchmaker_matches.allocation_id references a real allocation row.
	seedUnclaimedAlloc := func(i int) {
		var allocID int64
		require.NoError(t, c.bootstrapPool.QueryRow(ctx,
			`INSERT INTO game_server_allocations (tenant_id, project_id, backend, status, fleet_id)
			 VALUES ($1, $2, 'stub', 'ready', $3) RETURNING id`,
			tenantID, projectID, fleetID).Scan(&allocID))
		_, serr := c.bootstrapPool.Exec(ctx,
			`INSERT INTO matchmaker_matches (id, tenant_id, project_id, mode, allocation_id, roster, expires_at)
			 VALUES ($1, $2, $3, 'fleet_allocation', $4,
			         jsonb_build_array(jsonb_build_object('player_id', $5::bigint)),
			         now() + interval '1 hour')`,
			fmt.Sprintf("mm_cap_%d", i), tenantID, projectID, allocID, playerID)
		require.NoError(t, serr)
	}
	for i := 0; i < 3; i++ {
		seedUnclaimedAlloc(i)
	}

	// At the cap, a new fleet ticket is refused.
	assert.Equal(t, http.StatusTooManyRequests, createTicket())

	// Freeing one slot (the allocation expired) lets the next request through.
	_, err = c.bootstrapPool.Exec(ctx,
		`UPDATE matchmaker_matches SET expires_at = now() - interval '1 hour' WHERE id = 'mm_cap_0'`)
	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, createTicket())
}
