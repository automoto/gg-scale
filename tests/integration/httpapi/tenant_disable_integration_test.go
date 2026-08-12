//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postControlPanel posts a form with the given session cookie and CSRF field.
func postControlPanel(t *testing.T, baseURL, path string, cookie *http.Cookie, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func getControlPanel(t *testing.T, baseURL, path string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func tenantDisabledState(t *testing.T, c *cluster, tenantID int64) (disabledAt, disabledBy *string) {
	t.Helper()
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at::text, disabled_by FROM tenants WHERE id = $1`, tenantID).
		Scan(&disabledAt, &disabledBy))
	return disabledAt, disabledBy
}

func TestTenantDisable_selfservice_blocks_keys_and_reenable_restores(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-dis")
	adminID := seedControlPanelUser(t, c, "tadmin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	memberID := seedControlPanelUser(t, c, "tmember@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	srv, _ := newControlPanelAndPlayerServer(t, c)

	// Baseline: the key resolves, players can play.
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-dis")
	require.NotEmpty(t, tok)

	settingsPath := "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) + "/settings"

	// Negative authz: a member cannot disable the tenant.
	memberCookie, memberCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "tmember@example.com", "correct-horse-battery-staple")
	status, _ := postControlPanel(t, srv.URL, settingsPath+"/disable", memberCookie,
		url.Values{"_csrf": {memberCSRF}})
	assert.Equal(t, http.StatusForbidden, status, "member must not disable the tenant")
	disabledAt, _ := tenantDisabledState(t, c, tenantID)
	require.Nil(t, disabledAt)

	// The settings page offers the disable action to the tenant admin.
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "tadmin@example.com", "correct-horse-battery-staple")
	status, page := getControlPanel(t, srv.URL, settingsPath, adminCookie)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, page, settingsPath+"/disable")

	// Tenant admin disables.
	status, _ = postControlPanel(t, srv.URL, settingsPath+"/disable", adminCookie,
		url.Values{"_csrf": {adminCSRF}})
	require.Equal(t, http.StatusSeeOther, status)
	disabledAt, disabledBy := tenantDisabledState(t, c, tenantID)
	require.NotNil(t, disabledAt)
	require.NotNil(t, disabledBy)
	assert.Equal(t, "tenant", *disabledBy)

	// API keys stop resolving: player traffic is blocked with a non-enumerating 403.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "key-dis", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	// Tenant admin keeps control-panel access and can re-enable.
	status, page = getControlPanel(t, srv.URL, settingsPath, adminCookie)
	require.Equal(t, http.StatusOK, status, "tenant admin must keep control-panel access after self-disable")
	assert.Contains(t, page, settingsPath+"/enable")
	status, _ = postControlPanel(t, srv.URL, settingsPath+"/enable", adminCookie,
		url.Values{"_csrf": {adminCSRF}})
	require.Equal(t, http.StatusSeeOther, status)
	disabledAt, _ = tenantDisabledState(t, c, tenantID)
	assert.Nil(t, disabledAt)

	// Player traffic works again.
	tok, _ = anonymousLoginWithID(t, srv.URL, "key-dis")
	require.NotEmpty(t, tok)
}

func TestTenantDisable_platform_disable_locks_out_tenant_admin(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-plat")
	adminID := seedControlPanelUser(t, c, "t2admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	seedControlPanelUser(t, c, "plat@example.com", "correct-horse-battery-staple", true)
	srv, _ := newControlPanelAndPlayerServer(t, c)

	settingsPath := "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) + "/settings"

	// The tenant self-disables first: a platform disable must supersede it
	// in place (promote disabled_by) without a re-enable window.
	adminCookiePre, adminCSRFPre := controlPanelLoginCookieAndCSRF(t, srv.URL, "t2admin@example.com", "correct-horse-battery-staple")
	status, _ := postControlPanel(t, srv.URL, settingsPath+"/disable", adminCookiePre,
		url.Values{"_csrf": {adminCSRFPre}})
	require.Equal(t, http.StatusSeeOther, status)
	_, disabledBy := tenantDisabledState(t, c, tenantID)
	require.NotNil(t, disabledBy)
	require.Equal(t, "tenant", *disabledBy)

	// Platform admin disables the tenant (supersedes the self-disable).
	platCookie, platCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "plat@example.com", "correct-horse-battery-staple")
	status, _ = postControlPanel(t, srv.URL, settingsPath+"/disable", platCookie,
		url.Values{"_csrf": {platCSRF}})
	require.Equal(t, http.StatusSeeOther, status)
	disabledAtAfter, disabledBy := tenantDisabledState(t, c, tenantID)
	require.NotNil(t, disabledAtAfter, "tenant must never be re-enabled during the promotion")
	require.NotNil(t, disabledBy)
	assert.Equal(t, "platform", *disabledBy)

	// Player traffic is blocked.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "key-plat", nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	// The tenant admin is locked out of the tenant's pages...
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "t2admin@example.com", "correct-horse-battery-staple")
	status, _ = getControlPanel(t, srv.URL, settingsPath, adminCookie)
	assert.Equal(t, http.StatusForbidden, status, "platform disable must lock tenant admins out")

	// ...and cannot re-enable it (negative authz).
	status, _ = postControlPanel(t, srv.URL, settingsPath+"/enable", adminCookie,
		url.Values{"_csrf": {adminCSRF}})
	assert.Equal(t, http.StatusForbidden, status, "tenant admin must not re-enable a platform-disabled tenant")
	disabledAt, _ := tenantDisabledState(t, c, tenantID)
	require.NotNil(t, disabledAt, "tenant must stay disabled")

	// Only the platform admin can re-enable; tenant admin access returns.
	status, _ = postControlPanel(t, srv.URL, settingsPath+"/enable", platCookie,
		url.Values{"_csrf": {platCSRF}})
	require.Equal(t, http.StatusSeeOther, status)
	status, _ = getControlPanel(t, srv.URL, settingsPath, adminCookie)
	assert.Equal(t, http.StatusOK, status)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-plat")
	require.NotEmpty(t, tok)
}
