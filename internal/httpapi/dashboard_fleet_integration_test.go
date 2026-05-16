//go:build integration

package httpapi_test

import (
	"context"
	"errors"
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

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

// stubBackend is the deterministic in-process fleet.Backend the fleet UI
// integration tests run against. It records every Allocate/Deallocate so
// tests can assert the dashboard reached the backend, and the healthy /
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

func (s *stubBackend) Deallocate(_ context.Context, _ fleet.AllocationID, _ string) error {
	s.deallocateN.Add(1)
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

// newDashboardFleetServer wires the dashboard with a real PostgresStore-backed
// fleet.Manager pointed at the cluster, and the given backend stub.
func newDashboardFleetServer(t *testing.T, c *cluster, backend fleet.Backend, pluginInfo func() *dashboard.PluginSnapshot) (*httptest.Server, *fleet.Manager) {
	t.Helper()
	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	mgr := fleet.NewManager(
		fleet.NewPostgresStore(pool),
		fleet.NewPostgresFleetStore(pool),
		backend,
		fleet.ManagerOptions{Clock: func(int) time.Duration { return 0 }},
	)
	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    pool,
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer:  signer,
		Cache:   c.cache,
		Fleet:   mgr,
		Dashboard: dashboard.Config{
			Mount: true,
		},
		DashboardPluginInfo: pluginInfo,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, mgr
}

func TestDashboardFleet_list_then_allocate_then_appears_in_table(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-token-a")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) +
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
	// actor is a dashboard user, and the tenant_id rides along in payload).
	var auditCount int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'fleet.allocate.manual'
		   AND (payload->>'tenant_id')::bigint = $1`,
		tenantID).Scan(&auditCount))
	assert.Equal(t, 1, auditCount)
}

func TestDashboardFleet_detail_shows_pending_and_ready_events(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-token-events")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) +
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

func TestDashboardFleet_deallocate_rejects_wrong_typed_id_then_succeeds(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-token-d")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	fleetName := seedFleetTemplate(t, c, tenantID, projectID, backend.Name())

	base := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) +
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

func TestDashboardFleet_RLS_isolates_allocations_across_tenants(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-rls-a")
	tenantB, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-rls-b")
	aliceID := seedDashboardUser(t, c, "alice@example.com", "correct-horse-battery-staple", false)
	bobID := seedDashboardUser(t, c, "bob@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, aliceID, tenantA, "owner")
	seedDashboardMembership(t, c, bobID, tenantB, "owner")

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	fleetA := seedFleetTemplate(t, c, tenantA, projectA, backend.Name())

	// Alice allocates in tenant A.
	aliceCookie, aliceCSRF := dashboardLoginCookieAndCSRF(t, srv.URL, "alice@example.com", "correct-horse-battery-staple")
	aliceBase := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantA, 10) +
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
	bobCookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "bob@example.com", "correct-horse-battery-staple")
	crossReq, err := http.NewRequest(http.MethodGet, aliceBase, nil)
	require.NoError(t, err)
	crossReq.AddCookie(bobCookie)
	crossResp, err := http.DefaultClient.Do(crossReq)
	require.NoError(t, err)
	defer crossResp.Body.Close()
	assert.Equal(t, http.StatusForbidden, crossResp.StatusCode)

	// Bob viewing his own tenant's fleet sees no allocations (RLS proper).
	bobBase := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantB, 10) +
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

func TestDashboardFleet_backends_page_surfaces_unreachable_backend(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "fleet-token-h")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	backend := newStubBackend("stub")
	backend.healthy.Store(false)
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/fleet/backends", cookie)
	assert.Contains(t, body, "Backend unreachable")
	assert.Contains(t, body, "stub set to unhealthy")

	// Flip back to healthy and re-check.
	backend.healthy.Store(true)
	body = getWithCookie(t, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/fleet/backends", cookie)
	assert.Contains(t, body, "Backend healthy")
}

func TestDashboardMatchmaker_queue_lists_buckets_grouped_by_region(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "mm-token")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	// Seed an end_user we can foreign-key from the tickets table.
	var endUserID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO end_users (tenant_id, project_id, external_id, email, email_verified_at)
		 VALUES ($1, $2, 'ext-1', 'mm-player@example.com', now()) RETURNING id`,
		tenantID, projectID).Scan(&endUserID))
	// Seed 3 queued tickets across two (region, game_mode) buckets.
	for _, tup := range []struct{ region, mode string }{
		{"us-east-1", "ranked"}, {"us-east-1", "ranked"}, {"eu-1", "casual"},
	} {
		_, err := c.bootstrapPool.Exec(context.Background(),
			`INSERT INTO matchmaking_tickets (tenant_id, project_id, end_user_id, region, game_mode)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, projectID, endUserID, tup.region, tup.mode)
		require.NoError(t, err)
	}

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+
		"/projects/"+strconv.FormatInt(projectID, 10)+"/matchmaker", cookie)
	assert.Contains(t, body, "us-east-1")
	assert.Contains(t, body, "ranked")
	assert.Contains(t, body, "eu-1")
	assert.Contains(t, body, "casual")
	assert.Contains(t, body, "queued")
}

func TestDashboardPlugins_page_no_backend_shows_empty_state(t *testing.T) {
	c := startCluster(t)
	platformID := seedDashboardUser(t, c, "admin@example.com", "correct-horse-battery-staple", true)
	_ = platformID

	backend := newStubBackend("stub")
	srv, _ := newDashboardFleetServer(t, c, backend, nil)
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/dashboard/admin/plugins", cookie)
	assert.Contains(t, body, "No plugin backend configured")
}

func TestDashboardPlugins_page_renders_snapshot_when_provided(t *testing.T) {
	c := startCluster(t)
	_ = seedDashboardUser(t, c, "admin@example.com", "correct-horse-battery-staple", true)

	backend := newStubBackend("stub")
	snapshot := func() *dashboard.PluginSnapshot {
		return &dashboard.PluginSnapshot{
			Name: "ovh", Version: "1.2.3", ProtocolVersion: 1,
			Pid: 4242, RestartCount: 0, TotalRestartCount: 5,
		}
	}
	srv, _ := newDashboardFleetServer(t, c, backend, snapshot)
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	body := getWithCookie(t, srv.URL+"/v1/dashboard/admin/plugins", cookie)
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
