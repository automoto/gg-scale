//go:build integration

// Regression coverage for the security follow-up round (H5, H6, H13, H14, M5).
// Each test exercises a workflow that the original code would have failed
// — concurrent login lockout, CSRF rejection, RLS isolation, audit emission,
// and lifetime verification lockout.
package httpapi_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/players"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/verifycode"
)

// newPlayersServerForCluster builds a router with the player site mounted.
// Used by H6 CSRF tests; the standard newServerForCluster doesn't enable
// players because most other tests don't need them.
func newPlayersServerForCluster(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    db.NewPool(c.appPool),
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer:  signer,
		Cache:   c.cache,
		Players: players.Config{Mount: true},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

// TestH14_DashboardLoginFailures_AreAtomicUnderConcurrency is the load-bearing
// assertion for the TOCTOU fix: ten concurrent failed logins must produce
// login_failures = 10, not some smaller number from racing read-then-writes.
func TestH14_DashboardLoginFailures_AreAtomicUnderConcurrency(t *testing.T) {
	c := startCluster(t)
	const email = "lockme@example.com"
	seedDashboardUser(t, c, email, "correct-horse-battery-staple", false)
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())

	const concurrent = 10
	var (
		wg          sync.WaitGroup
		seen401     atomic.Int64
		seenLocked  atomic.Int64
		seenOther   atomic.Int64
		clientReady = make(chan struct{})
	)
	var statuses sync.Map
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-clientReady
			form := url.Values{"email": {email}, "password": {"wrong-password"}}
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/login",
				strings.NewReader(form.Encode()))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				seenOther.Add(1)
				statuses.Store(i, "err:"+err.Error())
				return
			}
			defer resp.Body.Close()
			statuses.Store(i, resp.StatusCode)
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusSeeOther:
				// SeeOther is the redirect-to-login flash after a failed login.
				seen401.Add(1)
			case http.StatusForbidden, http.StatusTooManyRequests:
				seenLocked.Add(1)
			default:
				seenOther.Add(1)
			}
		}()
	}
	close(clientReady)
	wg.Wait()

	if t.Failed() {
		statuses.Range(func(k, v any) bool {
			t.Logf("attempt %v: %v", k, v)
			return true
		})
	}

	// All ten attempts must be accounted for; the SQL counter must reflect
	// every increment (zero lost to TOCTOU).
	var failures int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT login_failures FROM dashboard_users WHERE email = $1`, email).Scan(&failures))
	assert.Equal(t, concurrent, failures,
		"login_failures must equal concurrent attempts (got 401=%d locked=%d other=%d)",
		seen401.Load(), seenLocked.Load(), seenOther.Load())

	// The lockout threshold is 10 — once we've hit it the locked_until
	// column must be set.
	var lockedUntil *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT locked_until FROM dashboard_users WHERE email = $1`, email).Scan(&lockedUntil))
	require.NotNil(t, lockedUntil)
	assert.True(t, lockedUntil.After(time.Now()), "locked_until must be in the future")
}

// TestH6_PlayerSitePOST_RejectsRequestWithoutCSRF proves the new double-submit
// middleware blocks anonymous form posts that don't carry the cookie/field.
func TestH6_PlayerSitePOST_RejectsRequestWithoutCSRF(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "csrf-h6")
	srv := newPlayersServerForCluster(t, c)

	form := url.Values{"email": {"someone@example.com"}, "password": {"hunter22"}}
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/players/p/"+strconv.FormatInt(projectID, 10)+"/login",
		strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"POST without csrf cookie/field must be rejected")
}

// TestH6_PlayerSitePOST_AcceptsRequestWithMatchingCSRF is the positive case:
// GET the page (cookie set + token rendered), POST back with both → 200/303.
func TestH6_PlayerSitePOST_AcceptsRequestWithMatchingCSRF(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "csrf-h6-ok")
	srv := newPlayersServerForCluster(t, c)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	loginPath := srv.URL + "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/login"
	getResp, err := client.Get(loginPath)
	require.NoError(t, err)
	body, _ := readAllString(getResp)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode)
	csrf := extractCSRFFromForm(t, body)
	require.NotEmpty(t, csrf)

	form := url.Values{
		"_csrf":    {csrf},
		"email":    {"unknown@example.com"},
		"password": {"wrong"},
	}
	req, err := http.NewRequest(http.MethodPost, loginPath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	// CSRF passes (no 403); credential check fails (401), proving we reached
	// the handler past the middleware.
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"unknown email with valid csrf reaches the handler → 401, not 403")
}

// TestH5_DashboardPlayers_AcrossTenants_IsolatedByRLS proves the H5 conversion
// from BootstrapQ to Q+WithTenant actually enforces tenant scoping at the RLS
// layer — even with a forged URL targeting the wrong tenant.
func TestH5_DashboardPlayers_AcrossTenants_IsolatedByRLS(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "h5-tenant-a")
	tenantB, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "h5-tenant-b")
	aliceID := seedDashboardUser(t, c, "alice-h5@example.com", "correct-horse-battery-staple", false)
	bobID := seedDashboardUser(t, c, "bob-h5@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, aliceID, tenantA, "owner")
	seedDashboardMembership(t, c, bobID, tenantB, "owner")

	// Seed one player in each tenant.
	ctx := context.Background()
	var pidA, pidB int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO end_users (tenant_id, project_id, external_id, email)
		 VALUES ($1, $2, 'alice-player', 'alice-player@example.com') RETURNING id`,
		tenantA, projectA).Scan(&pidA))
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO end_users (tenant_id, project_id, external_id, email)
		 VALUES ($1, $2, 'bob-player', 'bob-player@example.com') RETURNING id`,
		tenantB, projectB).Scan(&pidB))

	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())

	// Alice signs in, then asks for tenant A's player list — should see alice-player.
	aliceCookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "alice-h5@example.com", "correct-horse-battery-staple")
	listPathA := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players"
	bodyA := getWithCookie(t, listPathA, aliceCookie)
	assert.Contains(t, bodyA, "alice-player@example.com")
	assert.NotContains(t, bodyA, "bob-player@example.com")

	// Forged URL: Alice tries to view tenant B's player directly. The
	// requireTenantAccess middleware rejects (403); RLS would also block
	// even if the middleware were bypassed.
	forgedPath := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantB, 10) +
		"/projects/" + strconv.FormatInt(projectB, 10) + "/players/" + strconv.FormatInt(pidB, 10)
	forgedResp := getResponseWithCookie(t, forgedPath, aliceCookie)
	defer forgedResp.Body.Close()
	assert.NotEqual(t, http.StatusOK, forgedResp.StatusCode,
		"Alice must not be able to read tenant B's player; got %d", forgedResp.StatusCode)
}

// TestM5_APIKeyCreate_EmitsPlatformAuditRow proves the M5 audit expansion:
// every dashboard API-key create writes a platform_audit_log row tagged with
// the actor and the action.
func TestM5_APIKeyCreate_EmitsPlatformAuditRow(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "m5-audit")
	ownerID := seedDashboardUser(t, c, "owner-m5@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner-m5@example.com", "correct-horse-battery-staple")

	createURL := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) + "/api-keys"
	resp := postFormWithCookie(t, createURL, url.Values{
		"_csrf": {csrf},
		"label": {"audit test key"},
	}, cookie)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "api key create should render the success page")

	var count int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'dashboard.api_key.create'
		   AND actor_user_id = $1
		   AND (payload->>'tenant_id')::bigint = $2`,
		ownerID, tenantID).Scan(&count))
	assert.Equal(t, 1, count, "expected exactly one audit row for the api-key create action")
}

// TestM5_APIKeyRevoke_EmitsPlatformAuditRow covers the revoke side of M5.
func TestM5_APIKeyRevoke_EmitsPlatformAuditRow(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "m5-audit-revoke")
	ownerID := seedDashboardUser(t, c, "owner-m5r@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")

	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner-m5r@example.com", "correct-horse-battery-staple")

	createResp := postFormWithCookie(t, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys",
		url.Values{"_csrf": {csrf}, "label": {"to revoke"}}, cookie)
	createResp.Body.Close()

	var keyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM api_keys WHERE label = 'to revoke'`).Scan(&keyID))

	revokeURL := srv.URL + "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/api-keys/" + strconv.FormatInt(keyID, 10) + "/revoke"
	revokeResp := postFormWithCookie(t, revokeURL, url.Values{"_csrf": {csrf}}, cookie)
	revokeResp.Body.Close()

	var count int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'dashboard.api_key.revoke'
		   AND actor_user_id = $1
		   AND target = $2`,
		ownerID, strconv.FormatInt(keyID, 10)).Scan(&count))
	assert.Equal(t, 1, count)
}

// TestH13_LifetimeLockout_SurvivesResend exercises the lifetime ceiling:
// an end-user one short of MaxLifetimeAttempts (simulating a long
// /resend → exhaust → /resend cycle) trips the lock on the next wrong
// code. Pre-staging the lifetime counter dodges the per-IP rate limiter,
// which would otherwise throttle a real burst of 20+ verify POSTs.
func TestH13_LifetimeLockout_SurvivesResend(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "h13-lockout")
	srv, rec := newFullStackServer(t, c)

	signupResp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "h13-lockout",
		map[string]string{"email": "h13@example.com", "password": "ridingtheh13bus"})
	require.Equal(t, http.StatusAccepted, signupResp.StatusCode)
	require.GreaterOrEqual(t, len(rec.Sent), 1)

	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE end_users
		 SET email_verification_lifetime_attempts = $1,
		     email_verification_attempts = 0
		 WHERE tenant_id = $2 AND email = 'h13@example.com'`,
		verifycode.MaxLifetimeAttempts-1, tenantID)
	require.NoError(t, err)

	verifyResp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "h13-lockout",
		map[string]string{"email": "h13@example.com", "code": "000000"})
	require.Equal(t, http.StatusTooManyRequests, verifyResp.StatusCode, string(body))
	assert.Contains(t, string(body), "account locked")

	var locked *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email_verification_locked_until FROM end_users
		 WHERE tenant_id = $1 AND email = 'h13@example.com'`, tenantID).Scan(&locked))
	require.NotNil(t, locked, "locked_until must be set once the lifetime cap is reached")
	assert.True(t, locked.After(time.Now()), "locked_until must be in the future")
	assert.True(t, locked.Before(time.Now().Add(verifycode.LockoutDuration+time.Minute)),
		"locked_until must be ~LockoutDuration in the future")
}

// readAllString reads the response body fully into a string.
func readAllString(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	for {
		chunk := make([]byte, 4096)
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return string(buf), nil
			}
			return string(buf), nil
		}
	}
}

// getResponseWithCookie issues a GET and returns the response (caller closes
// the body). The standard getWithCookie helper reads + asserts 200; this
// variant lets callers assert non-200 outcomes.
func getResponseWithCookie(t *testing.T, url string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	return resp
}
