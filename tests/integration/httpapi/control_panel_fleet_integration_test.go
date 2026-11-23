//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
	"github.com/automoto/gg-scale/internal/tenant"
)

// stubBackend is the deterministic in-process fleet.Backend the fleet UI
// integration tests run against. It records every Allocate/Deallocate so
// tests can assert the control panel reached the backend, and the healthy /
// unreachable flag flips for the backends-page test.
type stubBackend struct {
	name        string
	healthy     atomic.Bool
	allocateN   atomic.Int64
	deallocateN atomic.Int64
}

func newStubBackend(name string) *stubBackend {
	s := &stubBackend{name: name}
	s.healthy.Store(true)
	return s
}

func (s *stubBackend) Name() string { return s.name }

func (s *stubBackend) Allocate(_ context.Context, _ fleet.AllocationRequest) (*fleet.Allocation, error) {
	n := s.allocateN.Add(1)
	return &fleet.Allocation{
		BackendRef: "ref-" + strconv.FormatInt(n, 10),
		Address:    "10.0.0." + strconv.FormatInt(n, 10) + ":7777",
	}, nil
}

// Deallocate fails for an empty ref, as the agones backend does.
func (s *stubBackend) Deallocate(_ context.Context, _ fleet.AllocationID, ref string) error {
	s.deallocateN.Add(1)
	if ref == "" {
		return errors.New("empty backend ref")
	}
	return nil
}

func (s *stubBackend) Status(_ context.Context, _ fleet.AllocationID, _ string) (fleet.Status, error) {
	return fleet.StatusReady, nil
}

func (s *stubBackend) Watch(_ context.Context, _ fleet.AllocationID, _ string) (<-chan fleet.StatusUpdate, error) {
	ch := make(chan fleet.StatusUpdate)
	close(ch)
	return ch, nil
}

func (s *stubBackend) HealthCheck(_ context.Context) error {
	if s.healthy.Load() {
		return nil
	}
	return errors.New("backend unreachable: stub set to unhealthy")
}

// seedFleetTemplate inserts a fleet row whose `backend` column matches the
// stub's Name() so manager.Allocate dispatches to it. Returns the fleet
// name (callers pass it as the form's "fleet" field).
func seedFleetTemplate(t *testing.T, c *cluster, tenantID, projectID int64, backendName string) string {
	t.Helper()
	name := "fleet-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, $3, $4, '{}'::jsonb)`,
		tenantID, projectID, name, backendName)
	require.NoError(t, err)
	return name
}

func seedFeatureGrant(t *testing.T, c *cluster, tenantID, projectID int64, feature rbac.Feature) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')
		 ON CONFLICT DO NOTHING`,
		tenantID, projectID, string(feature))
	require.NoError(t, err)
}

// newControlPanelFleetServer wires the control panel with a real PostgresStore-backed
// fleet.Manager pointed at the cluster, and the given backend stub.
func newControlPanelFleetServer(t *testing.T, c *cluster, backend fleet.Backend, pluginInfo func() *controlpanel.PluginSnapshot) (*httptest.Server, *fleet.Manager) {
	t.Helper()
	return newControlPanelFleetServerWithCipher(t, c, backend, pluginInfo, nil)
}

// newControlPanelFleetServerWithCipher is newControlPanelFleetServer with a
// credential cipher for the fleet store and the dashboard.
func newControlPanelFleetServerWithCipher(t *testing.T, c *cluster, backend fleet.Backend, pluginInfo func() *controlpanel.PluginSnapshot, cipher *secretseal.Cipher) (*httptest.Server, *fleet.Manager) {
	t.Helper()
	return newControlPanelFleetServerWithOptions(t, c, backend, pluginInfo, cipher,
		fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }, MaxCapacity: 10})
}

// newControlPanelFleetServerWithOptions is newControlPanelFleetServerWithCipher
// with the given manager options.
func newControlPanelFleetServerWithOptions(t *testing.T, c *cluster, backend fleet.Backend, pluginInfo func() *controlpanel.PluginSnapshot, cipher *secretseal.Cipher, opts fleet.ManagerOptions) (*httptest.Server, *fleet.Manager) {
	t.Helper()
	pool := db.NewPool(c.appPool)
	mgr := fleet.NewManager(
		fleet.NewPostgresStore(pool),
		fleet.NewPostgresFleetStore(pool, cipher),
		backend,
		opts,
	)
	return newControlPanelFleetServerWithManager(t, c, mgr, pluginInfo, cipher), mgr
}

// newControlPanelFleetServerWithManager serves the control panel with the
// given fleet manager.
func newControlPanelFleetServerWithManager(t *testing.T, c *cluster, mgr *fleet.Manager, pluginInfo func() *controlpanel.PluginSnapshot, cipher *secretseal.Cipher) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	router := httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		Pool:                  pool,
		Lookup:                tenant.NewSQLLookup(c.appPool),
		Limiter:               ratelimit.NewCacheLimiter(c.cache),
		Signer:                signer,
		Cache:                 c.cache,
		Fleet:                 mgr,
		RBAC:                  authorizer,
		CredentialCipher:      cipher,
		EmailVerifySigningKey: []byte(testEmailVerifySigningKey),
		ControlPanel: controlpanel.Config{
			Mount:        true,
			FleetEnabled: true,
		},
		ControlPanelPluginInfo: pluginInfo,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPanelFleet_list_then_allocate_then_appears_in_table(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-a")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

	// Empty state first.
	emptyBody := getWithCookie(t, base, cookie)
	assert.Contains(t, emptyBody, "No allocations")

	// Manual allocate.
	form := url.Values{
		"_csrf":     {csrf},
		"fleet":     {fleetName},
		"region":    {"us-east-1"},
		"game_mode": {"deathmatch"},
		"capacity":  {"4"},
	}
	allocResp := postFormWithCookie(t, base, form, cookie)
	defer allocResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, allocResp.StatusCode)
	require.Equal(t, int64(1), backend.allocateN.Load(), "manual allocate should reach the backend")

	// Allocation visible in the list table.
	listBody := getWithCookie(t, base, cookie)
	assert.Contains(t, listBody, "us-east-1")
	assert.Contains(t, listBody, "10.0.0.1:7777")
	assert.Contains(t, listBody, "ref-1")
	assert.Contains(t, listBody, "ready")

	// Audit row written for the manual allocate (platform_audit_log: the
	// actor is a control panel user, and the tenant_id rides along in payload).
	var auditCount int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'fleet.allocate.manual'
		   AND (payload->>'tenant_id')::bigint = $1`,
		tenantID).Scan(&auditCount))
	assert.Equal(t, 1, auditCount)
}

func TestControlPanelFleet_allocate_should_refuse_a_capacity_above_the_limit_with_a_form_error(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-cap")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())
	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

	resp := postFormWithCookie(t, base, url.Values{"_csrf": {csrf}, "fleet": {fleetName}, "region": {"eu"}, "capacity": {"11"}}, cookie)
	defer resp.Body.Close()

	assert.Equal(t, []any{http.StatusUnprocessableEntity, int64(0)}, []any{resp.StatusCode, backend.allocateN.Load()})
}

func TestControlPanelFleet_allocate_should_cap_the_live_allocations_of_the_project(t *testing.T) {
	cases := []struct {
		name       string
		live       int
		terminal   int
		wantStatus int
	}{
		{"should_allocate_under_the_cap", 1, 0, http.StatusSeeOther},
		{"should_refuse_at_the_cap_with_a_form_error", 2, 0, http.StatusConflict},
		{"should_not_count_terminal_allocations", 1, 2, http.StatusSeeOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-live-cap")
			ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
			seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
			seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
			seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
			backend := newStubBackend("agones")
			srv, _ := newControlPanelFleetServerWithOptions(t, c, backend, nil, nil,
				fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }, MaxCapacity: 10, MaxLiveAllocations: 2})
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
			fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())
			seed := func(n int, status string) {
				for range n {
					_, err := c.bootstrapPool.Exec(context.Background(),
						`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, region, status, released_at)
						 SELECT tenant_id, project_id, id, backend, 'eu', $2::allocation_status,
						        CASE WHEN $2 = 'shutdown' THEN now() END
						 FROM fleets WHERE name = $1`, fleetName, status)
					require.NoError(t, err)
				}
			}
			seed(tc.live, "ready")
			seed(tc.terminal, "shutdown")
			countRows := func() int {
				var n int
				require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
					`SELECT count(*) FROM game_server_allocations WHERE project_id = $1`, projectID).Scan(&n))
				return n
			}
			before := countRows()
			base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
				"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

			resp := postFormWithCookie(t, base, url.Values{"_csrf": {csrf}, "fleet": {fleetName}, "region": {"eu"}}, cookie)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			refused := tc.wantStatus == http.StatusConflict
			wantRows, wantCalls := before+1, int64(1)
			if refused {
				wantRows, wantCalls = before, 0
			}
			assert.Equal(t, []any{tc.wantStatus, wantCalls, wantRows, refused},
				[]any{resp.StatusCode, backend.allocateN.Load(), countRows(), strings.Contains(string(body), "FLEET_MAX_LIVE_ALLOCATIONS_PER_PROJECT")})
		})
	}
}

func TestControlPanelFleet_allocate_should_need_the_backend_grant(t *testing.T) {
	allGrants := []rbac.Feature{rbac.FeatureDedicatedServers, rbac.FeatureFleetAgonesBackend, rbac.FeatureFleetPluginBackend}
	cases := []struct {
		name       string
		backend    string
		grants     []rbac.Feature
		wantStatus int
		wantRows   int
	}{
		{"refused_without_backend_grant", "agones", []rbac.Feature{rbac.FeatureDedicatedServers}, http.StatusForbidden, 0},
		{"works_with_backend_grant", "agones", []rbac.Feature{rbac.FeatureDedicatedServers, rbac.FeatureFleetAgonesBackend}, http.StatusSeeOther, 1},
		// A stored backend that maps to no grant, run by a backend of the
		// same name so the manager would accept it.
		{"refused_for_a_backend_with_no_grant", "docker", allGrants, http.StatusForbidden, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-grant")
			ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
			seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
			for _, g := range tc.grants {
				seedFeatureGrant(t, c, tenantID, projectID, g)
			}
			backend := newStubBackend(tc.backend)
			srv, _ := newControlPanelFleetServer(t, c, backend, nil)
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
			fleetName := seedFleetTemplate(t, c, tenantID, projectID, tc.backend)
			base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
				"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

			resp := postFormWithCookie(t, base, url.Values{"_csrf": {csrf}, "fleet": {fleetName}, "region": {"eu"}}, cookie)
			defer resp.Body.Close()

			var rows int
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM game_server_allocations WHERE project_id = $1`, projectID).Scan(&rows))
			assert.Equal(t, []any{tc.wantStatus, int64(tc.wantRows), tc.wantRows}, []any{resp.StatusCode, backend.allocateN.Load(), rows})
		})
	}
}

func TestControlPanelFleet_detail_shows_pending_and_ready_events(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-events")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

	allocResp := postFormWithCookie(t, base, url.Values{
		"_csrf": {csrf}, "fleet": {fleetName}, "region": {"eu-1"}, "capacity": {"1"},
	}, cookie)
	defer allocResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, allocResp.StatusCode)

	var allocID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM game_server_allocations WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID).Scan(&allocID))

	detailBody := getWithCookie(t, base+"/"+strconv.FormatInt(allocID, 10), cookie)
	assert.Contains(t, detailBody, "Recent events")
	assert.Contains(t, detailBody, "pending")
	assert.Contains(t, detailBody, "ready")

	// Polled fragment endpoint returns the same data without the page chrome.
	fragmentBody := getWithCookie(t, base+"/"+strconv.FormatInt(allocID, 10)+"/events", cookie)
	assert.Contains(t, fragmentBody, "pending")
	assert.Contains(t, fragmentBody, "ready")
	assert.NotContains(t, fragmentBody, "<header", "fragment must not render page chrome")
}

func TestControlPanelFleet_deallocate_rejects_wrong_typed_id_then_succeeds(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-d")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"

	allocResp := postFormWithCookie(t, base, url.Values{
		"_csrf": {csrf}, "fleet": {fleetName}, "region": {"us-east-1"}, "capacity": {"1"},
	}, cookie)
	defer allocResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, allocResp.StatusCode)

	var allocID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM game_server_allocations WHERE tenant_id = $1`, tenantID).Scan(&allocID))

	deallocURL := base + "/" + strconv.FormatInt(allocID, 10) + "/deallocate"

	// Wrong typed ID → 422 + error rendered.
	wrongResp := postFormWithCookie(t, deallocURL, url.Values{
		"_csrf": {csrf}, "confirm_id": {strconv.FormatInt(allocID+999, 10)},
	}, cookie)
	defer wrongResp.Body.Close()
	wrongBody, err := io.ReadAll(wrongResp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, wrongResp.StatusCode)
	assert.Contains(t, string(wrongBody), "Typed ID did not match")
	assert.Equal(t, int64(0), backend.deallocateN.Load(), "wrong ID must not reach the backend")

	var stillLive bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT released_at IS NULL FROM game_server_allocations WHERE id = $1`, allocID).Scan(&stillLive))
	assert.True(t, stillLive)

	// Correct typed ID → 303 + DB row shutdown + backend called once.
	rightResp := postFormWithCookie(t, deallocURL, url.Values{
		"_csrf": {csrf}, "confirm_id": {strconv.FormatInt(allocID, 10)},
	}, cookie)
	defer rightResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, rightResp.StatusCode)
	assert.Equal(t, int64(1), backend.deallocateN.Load())

	var status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status::text FROM game_server_allocations WHERE id = $1`, allocID).Scan(&status))
	assert.Equal(t, "shutdown", status)

	var auditCount int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'fleet.deallocate.manual'
		   AND (payload->>'tenant_id')::bigint = $1`,
		tenantID).Scan(&auditCount))
	assert.Equal(t, 1, auditCount)
}

func TestControlPanelFleet_RLS_isolates_allocations_across_tenants(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-rls-a")
	tenantB, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-rls-b")
	aliceID := seedControlPanelUser(t, c, "alice@example.com", "correct-horse-battery-staple", false)
	bobID := seedControlPanelUser(t, c, "bob@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, aliceID, tenantA, "owner")
	seedControlPanelMembership(t, c, bobID, tenantB, "owner")
	seedFeatureGrant(t, c, tenantA, projectA, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantA, projectA, rbac.FeatureFleetAgonesBackend)

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	fleetA := seedFleetTemplate(t, c, tenantA, projectA, backend.Name())

	// Alice allocates in tenant A.
	aliceCookie, aliceCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "alice@example.com", "correct-horse-battery-staple")
	aliceBase := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/allocations"
	aliceAllocResp := postFormWithCookie(t, aliceBase, url.Values{
		"_csrf": {aliceCSRF}, "fleet": {fleetA}, "region": {"us-east-1"}, "capacity": {"1"},
	}, aliceCookie)
	defer aliceAllocResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, aliceAllocResp.StatusCode)
	var aliceAllocID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM game_server_allocations WHERE tenant_id = $1`, tenantA).Scan(&aliceAllocID))

	// Bob, in tenant B, hits tenant A's fleet path under his own cookie —
	// requireTenantAccess rejects with 403 BEFORE any RLS query.
	bobCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "bob@example.com", "correct-horse-battery-staple")
	crossReq, err := http.NewRequest(http.MethodGet, aliceBase, nil)
	require.NoError(t, err)
	crossReq.AddCookie(bobCookie)
	crossResp, err := http.DefaultClient.Do(crossReq)
	require.NoError(t, err)
	defer crossResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, crossResp.StatusCode)

	// Bob viewing his own tenant's fleet sees no allocations (RLS proper).
	bobBase := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantB, 10) +
		"/projects/" + strconv.FormatInt(projectB, 10) + "/allocations"
	bobListBody := getWithCookie(t, bobBase, bobCookie)
	assert.Contains(t, bobListBody, "No allocations")
	assert.NotContains(t, bobListBody, "ref-1", "Bob must not see Alice's backend_ref")

	// And requesting Alice's allocation detail under Bob's tenant URL 404s
	// (the handler enforces the project_id match before rendering).
	crossDetail := bobBase + "/" + strconv.FormatInt(aliceAllocID, 10)
	detailReq, err := http.NewRequest(http.MethodGet, crossDetail, nil)
	require.NoError(t, err)
	detailReq.AddCookie(bobCookie)
	detailResp, err := http.DefaultClient.Do(detailReq)
	require.NoError(t, err)
	defer detailResp.Body.Close()
	assert.Equal(t, http.StatusNotFound, detailResp.StatusCode)
}

func TestControlPanelFleet_backends_page_surfaces_unreachable_backend(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-h")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("agones")
	backend.healthy.Store(false)
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/fleet/backends", cookie)
	assert.Contains(t, body, "Backend unreachable")
	assert.Contains(t, body, "stub set to unhealthy")

	// Flip back to healthy and re-check.
	backend.healthy.Store(true)
	body = getWithCookie(t, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/fleet/backends", cookie)
	assert.Contains(t, body, "Backend healthy")
}

func TestControlPanelMatchmaker_queue_lists_buckets_grouped_by_region(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "mm-token")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")

	var fleetID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'mm-fleet', 'agones', '{}'::jsonb) RETURNING id`,
		tenantID, projectID).Scan(&fleetID))
	// Seed 3 queued tickets across two (region, game_mode) buckets. One
	// player per ticket: matchmaking_tickets_one_active_idx allows a player
	// only one queued ticket per project.
	for i, tup := range []struct{ region, mode string }{
		{"us-east-1", "ranked"}, {"us-east-1", "ranked"}, {"eu-1", "casual"},
	} {
		var playerID int64
		require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
			`INSERT INTO project_players (tenant_id, project_id, external_id, email, email_verified_at)
			 VALUES ($1, $2, $3, $4, now()) RETURNING id`,
			tenantID, projectID, fmt.Sprintf("ext-%d", i+1),
			fmt.Sprintf("mm-player-%d@example.com", i+1)).Scan(&playerID))
		_, err := c.bootstrapPool.Exec(context.Background(),
			`INSERT INTO matchmaking_tickets (tenant_id, project_id, fleet_id, player_id, region, game_mode)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, projectID, fleetID, playerID, tup.region, tup.mode)
		require.NoError(t, err)
	}

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+
		"/projects/"+strconv.FormatInt(projectID, 10)+"/matchmaker", cookie)
	assert.Contains(t, body, "us-east-1")
	assert.Contains(t, body, "ranked")
	assert.Contains(t, body, "eu-1")
	assert.Contains(t, body, "casual")
	assert.Contains(t, body, "queued")
}

// The dedicated-server view pages (fleet config, allocations, matchmaker
// queue) must require the per-object read scope, not just the coarse
// project:manage route gate. A tenant_admin holds project:manage and
// project:*:matchmaker read, but not project:*:fleet/allocation read, so it
// may view only the queue; an owner may view all three.
func TestControlPanelFleet_view_pages_require_object_read_scope(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-scope")

	// Seed memberships before the server so its authorizer loads them.
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)

	base := fmt.Sprintf("%s/v1/control-panel/tenants/%d/projects/%d", srv.URL, tenantID, projectID)
	pages := []string{base + "/matchmaker", base + "/allocations", base + "/fleets"}

	adminCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	assert.Equal(t, http.StatusOK, getStatusWithCookie(t, base+"/matchmaker", adminCookie),
		"tenant admin may view the matchmaker queue")
	for _, p := range pages[1:] {
		assert.Equal(t, http.StatusForbidden, getStatusWithCookie(t, p, adminCookie),
			"tenant admin must not view %s", p)
	}

	ownerCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	for _, p := range pages {
		assert.Equal(t, http.StatusOK, getStatusWithCookie(t, p, ownerCookie),
			"tenant owner may view %s", p)
	}
}

func getStatusWithCookie(t *testing.T, urlStr string, cookie *http.Cookie) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func TestControlPanelPlugins_page_no_backend_shows_empty_state(t *testing.T) {
	c := startCluster(t)
	platformID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", true)
	_ = platformID

	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServer(t, c, backend, nil)
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/control-panel/admin/plugins", cookie)
	assert.Contains(t, body, "No plugin backend configured")
}

func TestControlPanelPlugins_page_renders_snapshot_when_provided(t *testing.T) {
	c := startCluster(t)
	_ = seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", true)

	backend := newStubBackend("agones")
	snapshot := func() *controlpanel.PluginSnapshot {
		return &controlpanel.PluginSnapshot{
			Name: "ovh", Version: "1.2.3", ProtocolVersion: 1,
			Pid: 4242, RestartCount: 0, TotalRestartCount: 5,
		}
	}
	srv, _ := newControlPanelFleetServer(t, c, backend, snapshot)
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/control-panel/admin/plugins", cookie)
	assert.Contains(t, body, "ovh")
	assert.Contains(t, body, "1.2.3")
	assert.Contains(t, body, "4242")
	assert.Contains(t, body, "Health probe OK")
}

// helpers shared by the fleet integration tests.

func getWithCookie(t *testing.T, urlStr string, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, urlStr, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return string(body)
}

func postFormWithCookie(t *testing.T, urlStr string, form url.Values, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, urlStr, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	return resp
}

func TestControlPanelFleet_delete_then_restore_lists_the_fleet_again_and_audits_the_user(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-restore")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
	srv, _ := newControlPanelFleetServer(t, c, newStubBackend("agones"), nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	name := seedFleetTemplate(t, c, tenantID, projectID, "agones")
	var fleetID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM fleets WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&fleetID))
	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/fleets"
	one := base + "/" + strconv.FormatInt(fleetID, 10)

	del := postFormWithCookie(t, one+"/delete", url.Values{"_csrf": {csrf}}, cookie)
	del.Body.Close()
	deletedList := getWithCookie(t, base, cookie)
	restore := postFormWithCookie(t, one+"/restore", url.Values{"_csrf": {csrf}}, cookie)
	restore.Body.Close()
	restoredList := getWithCookie(t, base, cookie)

	var audited int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action IN ('fleet.template.delete', 'fleet.template.restore') AND target = $1 AND actor_user_id = $2`,
		strconv.FormatInt(fleetID, 10), ownerID).Scan(&audited))
	assert.Equal(t, []any{http.StatusSeeOther, true, http.StatusSeeOther, false, 2},
		[]any{del.StatusCode, strings.Contains(deletedList, "/restore"), restore.StatusCode, strings.Contains(restoredList, "/restore"), audited})
}

// With Agones project isolation on, the dashboard refuses the agones keys
// that the server sets, shows a field error, and writes nothing.
func TestControlPanelFleet_should_refuse_server_set_agones_keys_when_isolation_is_on(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-isolation")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
	srv, mgr := newControlPanelFleetServer(t, c, newStubBackend("agones"), nil)
	mgr.FleetAdmin().AgonesProjectIsolation = true
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	name := seedFleetTemplate(t, c, tenantID, projectID, "agones")
	var fleetID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM fleets WHERE tenant_id = $1 AND name = $2`, tenantID, name).Scan(&fleetID))
	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/fleets"
	forms := map[string]url.Values{
		"create namespace": {"name": {"new-ns"}, "backend": {"agones"}, "fleet_name": {"gs"}, "namespace": {"other-ns"}},
		"create selector":  {"name": {"new-sel"}, "backend": {"agones"}, "fleet_name": {"gs"}, "selector_key[]": {"ggscale.dev/project"}, "selector_value[]": {"99"}},
		"update namespace": {"name": {name}, "fleet_name": {"gs"}, "namespace": {"other-ns"}},
		"update selector":  {"name": {name}, "fleet_name": {"gs"}, "selector_key[]": {"ggscale.dev/project"}, "selector_value[]": {"99"}},
	}

	got := map[string][]any{}
	for label, form := range forms {
		form.Set("_csrf", csrf)
		target := base
		if strings.HasPrefix(label, "update") {
			target = base + "/" + strconv.FormatInt(fleetID, 10)
		}
		resp := postFormWithCookie(t, target, form, cookie)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		resp.Body.Close()
		field := `id="namespace-error"`
		if strings.HasSuffix(label, "selector") {
			field = `id="selector_key-error"`
		}
		got[label] = []any{resp.StatusCode, strings.Contains(string(body), field+">Agones project isolation is on")}
	}
	var rows int
	var stored string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*), max(config::text) FROM fleets WHERE tenant_id = $1`, tenantID).Scan(&rows, &stored))

	want := []any{http.StatusUnprocessableEntity, true}
	assert.Equal(t,
		map[string][]any{"create namespace": want, "create selector": want, "update namespace": want, "update selector": want, "rows": {1, "{}"}},
		map[string][]any{"create namespace": got["create namespace"], "create selector": got["create selector"],
			"update namespace": got["update namespace"], "update selector": got["update selector"], "rows": {rows, stored}})
}

// A pending row with no backend ref (the process stopped during the backend
// call) is released without a backend call, so it stops counting against
// the live allocation cap.
func TestControlPanelFleet_deallocate_should_release_a_pending_row_with_no_backend_ref(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-pending")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
	backend := newStubBackend("agones")
	srv, _ := newControlPanelFleetServerWithOptions(t, c, backend, nil, nil,
		fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }, MaxCapacity: 10, MaxLiveAllocations: 1})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())
	var stuck int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, region, status)
		 SELECT tenant_id, project_id, id, backend, 'eu', 'pending' FROM fleets WHERE name = $1 RETURNING id`,
		fleetName).Scan(&stuck))
	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"
	allocate := url.Values{"_csrf": {csrf}, "fleet": {fleetName}, "region": {"eu"}}

	atCap := postFormWithCookie(t, base, allocate, cookie)
	atCap.Body.Close()
	stop := postFormWithCookie(t, base+"/"+strconv.FormatInt(stuck, 10)+"/deallocate",
		url.Values{"_csrf": {csrf}, "confirm_id": {strconv.FormatInt(stuck, 10)}}, cookie)
	stop.Body.Close()
	var status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status::text FROM game_server_allocations WHERE id = $1`, stuck).Scan(&status))
	start := postFormWithCookie(t, base, allocate, cookie)
	start.Body.Close()

	assert.Equal(t, []any{http.StatusConflict, http.StatusSeeOther, "shutdown", int64(0), http.StatusSeeOther},
		[]any{atCap.StatusCode, stop.StatusCode, status, backend.deallocateN.Load(), start.StatusCode})
}

// A tenant_admin passes the project:manage route gate but has no fleet or
// allocation rights. Each POST handler must refuse it before it loads or
// shows stored data, also on the form-error paths.
func TestControlPanelFleet_post_handlers_should_refuse_a_tenant_admin_before_showing_stored_data(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-b1")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	for _, g := range []rbac.Feature{rbac.FeatureDedicatedServers, rbac.FeatureFleetAgonesBackend, rbac.FeatureFleetPluginBackend} {
		seedFeatureGrant(t, c, tenantID, projectID, g)
	}
	srv, _ := newControlPanelFleetServer(t, c, newStubBackend("agones"), nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	var agonesFleetID, pluginFleetID, allocID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'agones-marker-fleet', 'agones', '{"fleet_name":"gs-marker"}') RETURNING id`,
		tenantID, projectID).Scan(&agonesFleetID))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
		 VALUES ($1, $2, 'plugin-marker-fleet', 'plugin:markerbackend', '{"api_token":"token-marker"}') RETURNING id`,
		tenantID, projectID).Scan(&pluginFleetID))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO game_server_allocations (tenant_id, project_id, fleet_id, backend, backend_ref, region, address, status)
		 VALUES ($1, $2, $3, 'agones', 'ns-marker/gs-ref-marker', 'region-marker', '203.0.113.77:7777', 'ready') RETURNING id`,
		tenantID, projectID, agonesFleetID).Scan(&allocID))
	base := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10)
	markers := []string{"agones-marker-fleet", "plugin-marker-fleet", "markerbackend", "token-marker", "gs-ref-marker", "region-marker", "203.0.113.77"}
	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"allocate_with_an_empty_fleet", "/allocations", url.Values{"fleet": {""}, "region": {"eu"}}},
		{"deallocate_with_a_wrong_confirm_id", "/allocations/" + strconv.FormatInt(allocID, 10) + "/deallocate", url.Values{"confirm_id": {"0"}}},
		{"fleet_update_with_a_field_error", "/fleets/" + strconv.FormatInt(pluginFleetID, 10), url.Values{"name": {""}, "config_key[]": {"api_token"}, "config_value[]": {""}}},
		{"fleet_create_with_a_field_error", "/fleets", url.Values{"name": {""}, "backend": {"agones"}, "fleet_name": {"gs"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.form.Set("_csrf", csrf)

			resp := postFormWithCookie(t, base+tc.path, tc.form, cookie)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			resp.Body.Close()

			var shown []string
			for _, m := range markers {
				if strings.Contains(string(body), m) {
					shown = append(shown, m)
				}
			}
			assert.Equal(t, []any{http.StatusForbidden, []string(nil)}, []any{resp.StatusCode, shown}, string(body))
		})
	}
}

// failingLookupStore is a fleet store whose name lookup fails with a marker.
type failingLookupStore struct{ fleet.FleetStore }

func (failingLookupStore) GetByName(context.Context, int64, string) (*fleet.Fleet, error) {
	return nil, errors.New("lookup-store-marker")
}

// failingInsertStore is an allocation store whose insert fails with a marker.
type failingInsertStore struct{ fleet.Store }

func (failingInsertStore) InsertPending(context.Context, fleet.AllocationRequest, string) (fleet.AllocationID, error) {
	return 0, errors.New("insert-store-marker")
}

// newAllocateErrorServer serves the control panel for an owner with the
// grants, on a manager built from the given stores. It returns the
// allocations URL, the cookie, the CSRF value, and a live fleet name.
func newAllocateErrorServer(t *testing.T, store func(*db.Pool) fleet.Store, fleets func(*db.Pool) fleet.FleetStore) (string, *http.Cookie, string, string) {
	t.Helper()
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fleet-token-alloc-error")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureDedicatedServers)
	seedFeatureGrant(t, c, tenantID, projectID, rbac.FeatureFleetAgonesBackend)
	pool := db.NewPool(c.appPool)
	mgr := fleet.NewManager(store(pool), fleets(pool), newStubBackend("agones"),
		fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }, MaxCapacity: 10})
	srv := newControlPanelFleetServerWithManager(t, c, mgr, nil, nil)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	name := seedFleetTemplate(t, c, tenantID, projectID, "agones")
	return srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations", cookie, csrf, name
}

func postAllocate(t *testing.T, base string, cookie *http.Cookie, form url.Values) (int, string) {
	t.Helper()
	resp := postFormWithCookie(t, base, form, cookie)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// The allocate page shows a fixed text for an unknown error, never the
// error text.
func TestControlPanelFleet_allocate_should_not_show_store_error_text(t *testing.T) {
	plainStore := func(p *db.Pool) fleet.Store { return fleet.NewPostgresStore(p) }
	plainFleets := func(p *db.Pool) fleet.FleetStore { return fleet.NewPostgresFleetStore(p, nil) }
	cases := []struct {
		name   string
		store  func(*db.Pool) fleet.Store
		fleets func(*db.Pool) fleet.FleetStore
		marker string
	}{
		{"should_hide_a_fleet_lookup_error", plainStore,
			func(p *db.Pool) fleet.FleetStore { return failingLookupStore{plainFleets(p)} }, "lookup-store-marker"},
		{"should_hide_an_allocate_error", func(p *db.Pool) fleet.Store { return failingInsertStore{plainStore(p)} },
			plainFleets, "insert-store-marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, cookie, csrf, name := newAllocateErrorServer(t, tc.store, tc.fleets)

			status, body := postAllocate(t, base, cookie, url.Values{"_csrf": {csrf}, "fleet": {name}, "region": {"eu"}})

			assert.Equal(t, []any{http.StatusInternalServerError, false}, []any{status, strings.Contains(body, tc.marker)}, body)
		})
	}
}

// A fleet name that is not valid text or is too long is a field error. It
// never reaches the fleet lookup (the lookup here fails, so a 500 shows
// that it ran).
func TestControlPanelFleet_allocate_should_refuse_an_invalid_fleet_name_before_the_lookup(t *testing.T) {
	base, cookie, csrf, _ := newAllocateErrorServer(t,
		func(p *db.Pool) fleet.Store { return fleet.NewPostgresStore(p) },
		func(p *db.Pool) fleet.FleetStore { return failingLookupStore{fleet.NewPostgresFleetStore(p, nil)} })
	names := map[string]string{
		"nul":          "fl\x00eet",
		"invalid_utf8": "fl\xffeet",
		"too_long":     strings.Repeat("x", fleet.MaxConfigKeyOrVal+1),
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			status, body := postAllocate(t, base, cookie, url.Values{"_csrf": {csrf}, "fleet": {name}, "region": {"eu"}})

			assert.Equal(t, []any{http.StatusUnprocessableEntity, true},
				[]any{status, strings.Contains(body, `id="fleet-error">Fleet is not valid or too long.`)}, body)
		})
	}
}
