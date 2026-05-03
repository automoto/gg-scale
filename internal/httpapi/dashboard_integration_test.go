//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func newDashboardIntegrationServer(t *testing.T, c *cluster, bootstrap *dashboard.Bootstrap) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)

	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    db.NewPool(c.appPool),
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Signer:  signer,
		Cache:   c.cache,
		Dashboard: dashboard.Config{
			Mount: true,
		},
		DashboardBootstrap: bootstrap,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func TestDashboardSetup_creates_first_platform_admin_then_returns_410(t *testing.T) {
	c := startCluster(t)
	srv := newDashboardIntegrationServer(t, c, dashboard.NewBootstrap("setup-token"))
	noRedirect := noRedirectClient()

	form := url.Values{
		"bootstrap_token": {"setup-token"},
		"email":           {"Owner@Example.com"},
		"password":        {"correct-horse-battery-staple"},
	}
	resp, err := noRedirect.Post(srv.URL+"/v1/dashboard/setup", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/dashboard/login", resp.Header.Get("Location"))

	var isAdmin bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT is_platform_admin FROM dashboard_users WHERE email = $1`, "owner@example.com").Scan(&isAdmin))
	assert.True(t, isAdmin)

	goneResp, err := http.Get(srv.URL + "/v1/dashboard/setup?token=setup-token")
	require.NoError(t, err)
	defer goneResp.Body.Close()
	assert.Equal(t, http.StatusGone, goneResp.StatusCode)
}

func TestDashboardLogin_locks_account_after_10_failures(t *testing.T) {
	c := startCluster(t)
	userID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	require.Greater(t, userID, int64(0))
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())

	for range 10 {
		resp := dashboardLogin(t, srv.URL, "owner@example.com", "wrong-password")
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}

	resp := dashboardLogin(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusLocked, resp.StatusCode)
}

func TestDashboardCSRF_rejects_post_without_token(t *testing.T) {
	c := startCluster(t)
	seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	form := url.Values{
		"tenant_name":  {"Acme Games"},
		"project_name": {"Doomerang"},
		"label":        {"Default key"},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/tenants", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestDashboardCreateTenant_makes_creator_owner_and_writes_membership_and_audit(t *testing.T) {
	c := startCluster(t)
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	form := url.Values{
		"_csrf":        {csrf},
		"tenant_name":  {"Acme Games"},
		"project_name": {"Doomerang"},
		"label":        {"Default key"},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/tenants", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	body := string(rawBody)

	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	key := regexp.MustCompile(`ggs_[A-Za-z0-9_-]+`).FindString(body)
	require.NotEmpty(t, key)

	var tenantID, projectID, apiKeyID, membershipID int64
	var storedHash []byte
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(), `
		SELECT t.id, p.id, k.id, k.key_hash, m.id
		FROM tenants t
		JOIN projects p ON p.tenant_id = t.id
		JOIN api_keys k ON k.tenant_id = t.id AND k.project_id = p.id
		JOIN dashboard_memberships m ON m.tenant_id = t.id
		WHERE t.name = $1 AND p.name = $2 AND k.label = $3 AND m.dashboard_user_id = $4 AND m.role = 'owner'`,
		"Acme Games", "Doomerang", "Default key", ownerID,
	).Scan(&tenantID, &projectID, &apiKeyID, &storedHash, &membershipID))
	assert.Greater(t, membershipID, int64(0))

	sum := sha256.Sum256([]byte(key))
	assert.Equal(t, sum[:], storedHash)

	var auditPayload string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT payload::text FROM audit_log WHERE tenant_id = $1 AND action = 'dashboard.tenant.created'`,
		tenantID).Scan(&auditPayload))
	assert.Contains(t, auditPayload, strconv.FormatInt(ownerID, 10))
	assert.Contains(t, auditPayload, strconv.FormatInt(projectID, 10))
	assert.Contains(t, auditPayload, strconv.FormatInt(apiKeyID, 10))
}

func TestDashboardTenantIsolation_second_user_cannot_see_first_users_tenant(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "existing-key")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	otherID := seedDashboardUser(t, c, "other@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")
	require.Greater(t, otherID, int64(0))
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())

	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "other@example.com", "correct-horse-battery-staple")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestDashboardPlatformAdmin_sees_all_tenants(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "existing-key")
	seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, _ := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/dashboard", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "tenant-existing-key")
	assert.Contains(t, string(body), strconv.FormatInt(tenantID, 10))
}

func TestDashboardLogout_revokes_session_and_subsequent_request_redirects(t *testing.T) {
	c := startCluster(t)
	seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	form := url.Values{"_csrf": {csrf}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/logout", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	homeReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/dashboard", nil)
	require.NoError(t, err)
	homeReq.AddCookie(cookie)
	homeResp, err := noRedirect.Do(homeReq)
	require.NoError(t, err)
	defer homeResp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, homeResp.StatusCode)
	assert.Equal(t, "/v1/dashboard/login", homeResp.Header.Get("Location"))
}

func TestDashboardAPIKeys_create_label_and_revoke(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "existing-key")
	ownerID := seedDashboardUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedDashboardMembership(t, c, ownerID, tenantID, "owner")
	srv := newDashboardIntegrationServer(t, c, dashboard.DisabledBootstrap())
	cookie, csrf := dashboardLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	var existingKeyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM api_keys WHERE tenant_id = $1`, tenantID).Scan(&existingKeyID))

	listReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", nil)
	require.NoError(t, err)
	listReq.AddCookie(cookie)
	listResp, err := http.DefaultClient.Do(listReq)
	require.NoError(t, err)
	defer listResp.Body.Close()
	listBody, err := io.ReadAll(listResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listResp.StatusCode, string(listBody))
	assert.Contains(t, string(listBody), "API Keys")

	createForm := url.Values{
		"_csrf":      {csrf},
		"project_id": {strconv.FormatInt(projectID, 10)},
		"label":      {"new key"},
	}
	createReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", strings.NewReader(createForm.Encode()))
	require.NoError(t, err)
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.AddCookie(cookie)
	createResp, err := http.DefaultClient.Do(createReq)
	require.NoError(t, err)
	defer createResp.Body.Close()
	createBody, err := io.ReadAll(createResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, createResp.StatusCode, string(createBody))
	newKey := regexp.MustCompile(`ggs_[A-Za-z0-9_-]+`).FindString(string(createBody))
	require.NotEmpty(t, newKey)

	var storedHash []byte
	var newKeyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id, key_hash FROM api_keys WHERE tenant_id = $1 AND label = $2`,
		tenantID, "new key",
	).Scan(&newKeyID, &storedHash))
	sum := sha256.Sum256([]byte(newKey))
	assert.Equal(t, sum[:], storedHash)

	labelForm := url.Values{"_csrf": {csrf}, "label": {"renamed key"}}
	labelReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys/"+strconv.FormatInt(newKeyID, 10)+"/label", strings.NewReader(labelForm.Encode()))
	require.NoError(t, err)
	labelReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	labelReq.AddCookie(cookie)
	labelResp, err := noRedirect.Do(labelReq)
	require.NoError(t, err)
	defer labelResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, labelResp.StatusCode)

	var updatedLabel string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT label FROM api_keys WHERE id = $1`, newKeyID).Scan(&updatedLabel))
	assert.Equal(t, "renamed key", updatedLabel)

	bucketKey := ratelimit.APIKeyBucketKey(existingKeyID)
	allowed, _, err := c.cache.TokenBucket(context.Background(), bucketKey, 1, 0.01, 1)
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, _, err = c.cache.TokenBucket(context.Background(), bucketKey, 1, 0.01, 1)
	require.NoError(t, err)
	require.False(t, allowed)

	revokeForm := url.Values{"_csrf": {csrf}}
	revokeReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys/"+strconv.FormatInt(existingKeyID, 10)+"/revoke", strings.NewReader(revokeForm.Encode()))
	require.NoError(t, err)
	revokeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeReq.AddCookie(cookie)
	revokeResp, err := noRedirect.Do(revokeReq)
	require.NoError(t, err)
	defer revokeResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, revokeResp.StatusCode)

	var revoked bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, existingKeyID).Scan(&revoked))
	assert.True(t, revoked)

	allowed, _, err = c.cache.TokenBucket(context.Background(), bucketKey, 1, 0.01, 1)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func seedDashboardUser(t *testing.T, c *cluster, email, password string, platformAdmin bool) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	require.NoError(t, err)
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO dashboard_users (email, password_hash, is_platform_admin) VALUES ($1, $2, $3) RETURNING id`,
		email, hash, platformAdmin).Scan(&id))
	return id
}

func seedDashboardMembership(t *testing.T, c *cluster, userID, tenantID int64, role string) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO dashboard_memberships (dashboard_user_id, tenant_id, role) VALUES ($1, $2, $3)`,
		userID, tenantID, role)
	require.NoError(t, err)
}

func dashboardLogin(t *testing.T, baseURL, email, password string) *http.Response {
	t.Helper()
	form := url.Values{"email": {email}, "password": {password}}
	resp, err := noRedirectClient().Post(baseURL+"/v1/dashboard/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	return resp
}

func dashboardLoginCookieAndCSRF(t *testing.T, baseURL, email, password string) (*http.Cookie, string) {
	t.Helper()
	resp := dashboardLogin(t, baseURL, email, password)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/dashboard", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	homeResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer homeResp.Body.Close()
	body, err := io.ReadAll(homeResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, homeResp.StatusCode, string(body))

	csrf := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(string(body))
	require.Len(t, csrf, 2)
	return cookie, csrf[1]
}

func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
