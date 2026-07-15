//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var signupCodeRe = regexp.MustCompile(`request-access/accept\?code=([^\s]+)`)

// postForm sends an application/x-www-form-urlencoded POST, optionally with a
// session cookie, and returns the response (redirects are not followed).
func postForm(t *testing.T, client *http.Client, target string, form url.Values, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

func countSignupRequests(t *testing.T, c *cluster, email string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_signup_requests WHERE email = $1`, email).Scan(&n))
	return n
}

// TestTenantSignup_happy_path_creates_bare_tenant drives the full public
// sign-up flow: platform admin enables the toggle, a developer submits a
// request, the admin approves (editing the name), and accepting the emailed
// invite creates a tenant owned by the developer with no project.
func TestTenantSignup_happy_path_creates_bare_tenant(t *testing.T) {
	c := startCluster(t)
	adminID := seedControlPanelUser(t, c, "padmin@example.com", "correct-horse-battery-staple", true)
	_ = adminID
	srv, rec := newControlPanelAndPlayerServer(t, c)

	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "padmin@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	// Toggle defaults OFF: the public form is closed until enabled.
	closed := jarClient(t)
	resp, err := closed.Get(srv.URL + "/v1/control-panel/request-access")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, string(body), "closed")

	// Platform admin enables public sign-up.
	resp = postForm(t, noRedirect, srv.URL+"/v1/control-panel/admin/tenant-signups/config",
		url.Values{"_csrf": {adminCSRF}, "enabled": {"on"}}, adminCookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Developer submits a request (unauthenticated, CSRF via the form page).
	const devEmail = "dev@example.com"
	dev := jarClient(t)
	formCSRF := getPlayerCSRF(t, dev, srv.URL+"/v1/control-panel/request-access")
	resp = postForm(t, dev, srv.URL+"/v1/control-panel/request-access", url.Values{
		"_csrf":               {formCSRF},
		"email":               {devEmail},
		"tenant_name":         {"Abyssal Depths"},
		"project_description": {"A co-op roguelike diver."},
		"studio_name":         {"Deep Games"},
	}, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "Request received")

	var requestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM tenant_signup_requests WHERE email = $1 AND status = 'pending'`, devEmail).Scan(&requestID))

	// Admin approves, correcting the tenant name.
	resp = postForm(t, noRedirect, srv.URL+"/v1/control-panel/admin/tenant-signups/"+strconv.FormatInt(requestID, 10)+"/approve",
		url.Values{"_csrf": {adminCSRF}, "tenant_name": {"Abyssal Depths Reborn"}}, adminCookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// The approval email carries the accept link; pull the code out of it.
	require.NotEmpty(t, rec.Sent)
	last := rec.Sent[len(rec.Sent)-1]
	require.Equal(t, []string{devEmail}, last.To)
	m := signupCodeRe.FindStringSubmatch(last.Body)
	require.Len(t, m, 2, "accept code not found in email: %s", last.Body)
	code, err := url.QueryUnescape(m[1])
	require.NoError(t, err)

	// Developer accepts, setting a password — this creates the tenant.
	acceptURL := srv.URL + "/v1/control-panel/request-access/accept?code=" + url.QueryEscape(code)
	accept := jarClient(t)
	acceptCSRF := getPlayerCSRF(t, accept, acceptURL)
	resp = postForm(t, accept, srv.URL+"/v1/control-panel/request-access/accept", url.Values{
		"_csrf":    {acceptCSRF},
		"code":     {code},
		"password": {"correct-horse-battery-staple"},
	}, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// A tenant was created, owned by the new developer, with NO project.
	var tenantID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM tenants WHERE name = 'Abyssal Depths Reborn' AND deleted_at IS NULL`).Scan(&tenantID))
	var enforceQuotas bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT enforce_quotas FROM tenants WHERE id = $1`, tenantID).Scan(&enforceQuotas))
	assert.False(t, enforceQuotas, "zero-config self-host tenant signups stay uncapped")

	var ownerCount int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM control_panel_memberships m
		 JOIN control_panel_users u ON u.id = m.control_panel_user_id
		 WHERE m.tenant_id = $1 AND m.role = 'owner' AND u.email = $2`, tenantID, devEmail).Scan(&ownerCount))
	assert.Equal(t, int64(1), ownerCount, "developer should own the new tenant")

	var projectCount int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects WHERE tenant_id = $1`, tenantID).Scan(&projectCount))
	assert.Equal(t, int64(0), projectCount, "public signup creates no project")

	var status string
	var reqTenantID *int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status, tenant_id FROM tenant_signup_requests WHERE id = $1`, requestID).Scan(&status, &reqTenantID))
	assert.Equal(t, "accepted", status)
	require.NotNil(t, reqTenantID)
	assert.Equal(t, tenantID, *reqTenantID)

	// One-owned-tenant: a second submit for the same email is silently ignored
	// (anti-enumeration acknowledgement, but no new row).
	dev2 := jarClient(t)
	csrf2 := getPlayerCSRF(t, dev2, srv.URL+"/v1/control-panel/request-access")
	resp = postForm(t, dev2, srv.URL+"/v1/control-panel/request-access", url.Values{
		"_csrf":               {csrf2},
		"email":               {devEmail},
		"tenant_name":         {"Second Tenant Attempt"},
		"project_description": {"Another game."},
	}, nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "Request received")
	assert.Equal(t, int64(1), countSignupRequests(t, c, devEmail), "no second request row for the same email")
}

// TestTenantSignup_deny_blocks_reapply proves a denied email can't re-apply:
// the denied row keeps the email's unique slot, so a later submit is a silent
// no-op that creates no new pending request.
func TestTenantSignup_deny_blocks_reapply(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "padmin@example.com", "correct-horse-battery-staple", true)
	srv, _ := newControlPanelAndPlayerServer(t, c)

	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "padmin@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	postForm(t, noRedirect, srv.URL+"/v1/control-panel/admin/tenant-signups/config",
		url.Values{"_csrf": {adminCSRF}, "enabled": {"on"}}, adminCookie).Body.Close()

	const email = "rejected@example.com"
	dev := jarClient(t)
	csrf := getPlayerCSRF(t, dev, srv.URL+"/v1/control-panel/request-access")
	postForm(t, dev, srv.URL+"/v1/control-panel/request-access", url.Values{
		"_csrf":               {csrf},
		"email":               {email},
		"tenant_name":         {"Rejected Studio"},
		"project_description": {"A game."},
	}, nil).Body.Close()

	var requestID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM tenant_signup_requests WHERE email = $1`, email).Scan(&requestID))

	resp := postForm(t, noRedirect, srv.URL+"/v1/control-panel/admin/tenant-signups/"+strconv.FormatInt(requestID, 10)+"/deny",
		url.Values{"_csrf": {adminCSRF}, "reason": {"Not a fit right now."}}, adminCookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status FROM tenant_signup_requests WHERE id = $1`, requestID).Scan(&status))
	assert.Equal(t, "denied", status)

	// Re-apply with the same email: silent no-op, still exactly one (denied) row.
	dev2 := jarClient(t)
	csrf2 := getPlayerCSRF(t, dev2, srv.URL+"/v1/control-panel/request-access")
	resp = postForm(t, dev2, srv.URL+"/v1/control-panel/request-access", url.Values{
		"_csrf":               {csrf2},
		"email":               {email},
		"tenant_name":         {"Trying Again"},
		"project_description": {"A game."},
	}, nil)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), countSignupRequests(t, c, email))
}
