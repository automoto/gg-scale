//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"fmt"
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
	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

func newControlPanelIntegrationServer(t *testing.T, c *cluster, bootstrap *controlpanel.Bootstrap) *httptest.Server {
	t.Helper()
	srv, _ := newControlPanelIntegrationServerWithMailer(t, c, bootstrap)
	return srv
}

func newControlPanelIntegrationServerWithMailer(t *testing.T, c *cluster, bootstrap *controlpanel.Bootstrap) (*httptest.Server, *mailer.Recorder) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	rec := &mailer.Recorder{}
	router := httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		Pool:                  pool,
		Lookup:                tenant.NewSQLLookup(c.appPool),
		Signer:                signer,
		Cache:                 c.cache,
		RBAC:                  authorizer,
		Mailer:                rec,
		EmailVerifySigningKey: []byte(testEmailVerifySigningKey),
		ControlPanel: controlpanel.Config{
			Mount:    true,
			MailFrom: "noreply@ggscale.test",
		},
		ControlPanelBootstrap: bootstrap,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestControlPanelSetup_creates_first_platform_admin_then_returns_410(t *testing.T) {
	c := startCluster(t)
	srv, rec := newControlPanelIntegrationServerWithMailer(t, c, controlpanel.NewBootstrap("setup-token", "/tmp/ggscale-bootstrap.token"))
	noRedirect := noRedirectClient()

	form := url.Values{
		"bootstrap_token": {"setup-token"},
		"email":           {"Owner@Example.com"},
		"password":        {"correct-horse-battery-staple"},
	}
	resp, err := noRedirect.Post(srv.URL+"/v1/control-panel/setup", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer resp.Body.Close()

	// setup lands on the verify screen (not a second login)
	// with a verification email already sent.
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/control-panel/verify", resp.Header.Get("Location"))

	require.Len(t, rec.Sent, 1)
	assert.Equal(t, []string{"owner@example.com"}, rec.Sent[0].To)
	assert.Contains(t, rec.Sent[0].Subject, "verification code")

	var isAdmin bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT is_platform_admin FROM control_panel_users WHERE email = $1`, "owner@example.com").Scan(&isAdmin))
	assert.True(t, isAdmin)

	// Tenant-scope platform-admin capability comes from the Casbin grouping
	// row, so bootstrap must write it alongside the is_platform_admin column.
	var groupingRows int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM casbin_rule
		 WHERE ptype = 'g'
		   AND v0 = 'control_panel:user:' || (SELECT id FROM control_panel_users WHERE email = $1)
		   AND v1 = 'role:platform_admin' AND v2 = '*'`, "owner@example.com").Scan(&groupingRows))
	assert.Equal(t, 1, groupingRows, "bootstrap admin must hold the platform-admin grouping row")

	goneResp, err := http.Get(srv.URL + "/v1/control-panel/setup?token=setup-token")
	require.NoError(t, err)
	defer goneResp.Body.Close()
	assert.Equal(t, http.StatusGone, goneResp.StatusCode)
}

func TestControlPanelLogin_locks_account_after_10_failures(t *testing.T) {
	c := startCluster(t)
	userID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	require.Greater(t, userID, int64(0))
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())

	for range 10 {
		resp := controlPanelLogin(t, srv.URL, "owner@example.com", "wrong-password")
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}

	resp := controlPanelLogin(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusLocked, resp.StatusCode)
}

func TestControlPanelCSRF_rejects_post_without_token(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	form := url.Values{
		"tenant_name":  {"Acme Games"},
		"project_name": {"Doomerang"},
		"label":        {"Default key"},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/tenants", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestControlPanelCreateTenant_makes_creator_owner_and_writes_membership_and_audit(t *testing.T) {
	c := startCluster(t)
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	form := url.Values{
		"_csrf":        {csrf},
		"tenant_name":  {"Acme Games"},
		"project_name": {"Doomerang"},
		"label":        {"Default key"},
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/tenants", strings.NewReader(form.Encode()))
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
		JOIN control_panel_memberships m ON m.tenant_id = t.id
		WHERE t.name = $1 AND p.name = $2 AND k.label = $3 AND m.control_panel_user_id = $4 AND m.role = 'owner'`,
		"Acme Games", "Doomerang", "Default key", ownerID,
	).Scan(&tenantID, &projectID, &apiKeyID, &storedHash, &membershipID))
	assert.Greater(t, membershipID, int64(0))

	sum := sha256.Sum256([]byte(key))
	assert.Equal(t, sum[:], storedHash)

	var auditPayload string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT payload::text FROM audit_log WHERE tenant_id = $1 AND action = 'control_panel.tenant.created'`,
		tenantID).Scan(&auditPayload))
	assert.Contains(t, auditPayload, strconv.FormatInt(ownerID, 10))
	assert.Contains(t, auditPayload, strconv.FormatInt(projectID, 10))
	assert.Contains(t, auditPayload, strconv.FormatInt(apiKeyID, 10))
}

func TestControlPanelTenantIsolation_second_user_cannot_see_first_users_tenant(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-key")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	otherID := seedControlPanelUser(t, c, "other@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	require.Greater(t, otherID, int64(0))
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())

	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "other@example.com", "correct-horse-battery-staple")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestControlPanelRBAC_member_cannot_manage_tenant_admin_routes(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "member-key")
	memberID := seedControlPanelUser(t, c, "member@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())

	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "member@example.com", "correct-horse-battery-staple")
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestControlPanelPlatformAdmin_sees_all_tenants(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-key")
	seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel", nil)
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

func TestControlPanelPlatformAdmin_manages_foreign_tenant_team_and_keys(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-key")
	// Platform admin with no membership in any tenant: tenant-scope access
	// must come entirely from the Casbin platform_admin policy.
	seedControlPanelUser(t, c, "platform@example.com", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "platform@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()
	tenantPath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10)

	teamReq, err := http.NewRequest(http.MethodGet, tenantPath+"/team", nil)
	require.NoError(t, err)
	teamReq.AddCookie(cookie)
	teamResp, err := http.DefaultClient.Do(teamReq)
	require.NoError(t, err)
	defer teamResp.Body.Close()
	assert.Equal(t, http.StatusOK, teamResp.StatusCode, "platform admin loads a foreign tenant's team page")

	inviteForm := url.Values{
		"_csrf": {csrf},
		"email": {"invitee@example.com"},
		"role":  {"tenant_member"},
	}
	inviteReq, err := http.NewRequest(http.MethodPost, tenantPath+"/team/invite", strings.NewReader(inviteForm.Encode()))
	require.NoError(t, err)
	inviteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	inviteReq.AddCookie(cookie)
	inviteResp, err := noRedirect.Do(inviteReq)
	require.NoError(t, err)
	defer inviteResp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, inviteResp.StatusCode, "platform admin invites into a foreign tenant")

	listReq, err := http.NewRequest(http.MethodGet, tenantPath+"/api-keys", nil)
	require.NoError(t, err)
	listReq.AddCookie(cookie)
	listResp, err := http.DefaultClient.Do(listReq)
	require.NoError(t, err)
	defer listResp.Body.Close()
	assert.Equal(t, http.StatusOK, listResp.StatusCode, "platform admin lists a foreign tenant's API keys")

	createForm := url.Values{
		"_csrf":      {csrf},
		"project_id": {strconv.FormatInt(projectID, 10)},
		"key_type":   {"secret"},
		"label":      {"platform-created key"},
	}
	createReq, err := http.NewRequest(http.MethodPost, tenantPath+"/api-keys", strings.NewReader(createForm.Encode()))
	require.NoError(t, err)
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.AddCookie(cookie)
	createResp, err := http.DefaultClient.Do(createReq)
	require.NoError(t, err)
	defer createResp.Body.Close()
	createBody, err := io.ReadAll(createResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, createResp.StatusCode, string(createBody))
	require.NotEmpty(t, regexp.MustCompile(`ggs_[A-Za-z0-9_-]+`).FindString(string(createBody)))

	var newKeyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM api_keys WHERE tenant_id = $1 AND label = $2`,
		tenantID, "platform-created key").Scan(&newKeyID))

	revokeForm := url.Values{"_csrf": {csrf}}
	revokeReq, err := http.NewRequest(http.MethodPost, tenantPath+"/api-keys/"+strconv.FormatInt(newKeyID, 10)+"/revoke", strings.NewReader(revokeForm.Encode()))
	require.NoError(t, err)
	revokeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeReq.AddCookie(cookie)
	revokeResp, err := noRedirect.Do(revokeReq)
	require.NoError(t, err)
	defer revokeResp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, revokeResp.StatusCode, "platform admin revokes a foreign tenant's API key")
}

func TestControlPanelTenantAdmin_cannot_manage_other_tenants_team(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "target-key")
	otherTenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "other-key")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, otherTenantID, "admin")
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	inviteForm := url.Values{
		"_csrf": {csrf},
		"email": {"invitee@example.com"},
		"role":  {"tenant_member"},
	}
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/team/invite",
		strings.NewReader(inviteForm.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "tenant admin of one tenant must not manage another tenant's team")
}

func TestControlPanelLogout_revokes_session_and_subsequent_request_redirects(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	form := url.Values{"_csrf": {csrf}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/logout", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	homeReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel", nil)
	require.NoError(t, err)
	homeReq.AddCookie(cookie)
	homeResp, err := noRedirect.Do(homeReq)
	require.NoError(t, err)
	defer homeResp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, homeResp.StatusCode)
	assert.Equal(t, "/v1/control-panel/login", homeResp.Header.Get("Location"))
}

func TestControlPanelAPIKeys_create_label_and_revoke(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "existing-key")
	ownerID := seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, ownerID, tenantID, "owner")
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")
	noRedirect := noRedirectClient()

	var existingKeyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM api_keys WHERE tenant_id = $1`, tenantID).Scan(&existingKeyID))

	listReq, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", nil)
	require.NoError(t, err)
	listReq.AddCookie(cookie)
	listResp, err := http.DefaultClient.Do(listReq)
	require.NoError(t, err)
	defer listResp.Body.Close()
	listBody, err := io.ReadAll(listResp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listResp.StatusCode, string(listBody))
	assert.Contains(t, string(listBody), "API keys")

	createForm := url.Values{
		"_csrf":      {csrf},
		"project_id": {strconv.FormatInt(projectID, 10)},
		"key_type":   {"secret"},
		"label":      {"new key"},
	}
	createReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys", strings.NewReader(createForm.Encode()))
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
	labelReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys/"+strconv.FormatInt(newKeyID, 10)+"/label", strings.NewReader(labelForm.Encode()))
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
	revokeReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/control-panel/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys/"+strconv.FormatInt(existingKeyID, 10)+"/revoke", strings.NewReader(revokeForm.Encode()))
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

func TestControlPanelAPIKeyMutations_authorizeAgainstKeyType(t *testing.T) {
	tests := []struct {
		name          string
		actorRole     string
		keyType       string
		foreignTenant bool
		wantStatus    int
	}{
		{name: "tenant_admin_publishable", actorRole: "admin", keyType: "publishable", wantStatus: http.StatusSeeOther},
		{name: "tenant_admin_secret", actorRole: "admin", keyType: "secret", wantStatus: http.StatusForbidden},
		{name: "owner_secret", actorRole: "owner", keyType: "secret", wantStatus: http.StatusSeeOther},
		{name: "other_tenant", actorRole: "owner", keyType: "secret", foreignTenant: true, wantStatus: http.StatusNotFound},
	}
	operations := []struct {
		name       string
		pathSuffix string
		form       func(string) url.Values
	}{
		{
			name:       "relabel",
			pathSuffix: "/label",
			form: func(csrf string) url.Values {
				return url.Values{"_csrf": {csrf}, "label": {"renamed"}}
			},
		},
		{
			name:       "scope_update",
			pathSuffix: "/features",
			form: func(csrf string) url.Values {
				return url.Values{"_csrf": {csrf}, "scopes": {tenant.ScopeMatchmaker}}
			},
		},
		{
			name:       "revoke",
			pathSuffix: "/revoke",
			form: func(csrf string) url.Values {
				return url.Values{"_csrf": {csrf}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			actorTenantID, actorProjectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "actor-"+tc.name)
			targetTenantID, targetProjectID := actorTenantID, actorProjectID
			if tc.foreignTenant {
				targetTenantID, targetProjectID = seedTenantWithAPIKey(t, c.bootstrapPool, 0, "target-"+tc.name)
			}
			actorID := seedControlPanelUser(t, c, "actor@example.com", "correct-horse-battery-staple", false)
			seedControlPanelMembership(t, c, actorID, actorTenantID, tc.actorRole)
			srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "actor@example.com", "correct-horse-battery-staple")
			client := noRedirectClient()

			for i, operation := range operations {
				t.Run(operation.name, func(t *testing.T) {
					keyID := seedControlPanelMutationAPIKey(t, c, targetTenantID, targetProjectID, tc.keyType, i)
					form := operation.form(csrf)
					path := fmt.Sprintf("%s/v1/control-panel/tenants/%d/api-keys/%d%s",
						srv.URL, actorTenantID, keyID, operation.pathSuffix)
					req, err := http.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.AddCookie(cookie)

					resp, err := client.Do(req)
					require.NoError(t, err)
					defer resp.Body.Close()

					assert.Equal(t, tc.wantStatus, resp.StatusCode)
				})
			}
		})
	}
}

func seedControlPanelMutationAPIKey(t *testing.T, c *cluster, tenantID, projectID int64, keyType string, index int) int64 {
	t.Helper()
	tokenHash := sha256.Sum256([]byte(fmt.Sprintf("mutation-%d-%d-%s-%d", tenantID, projectID, keyType, index)))
	var keyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, scopes)
		 VALUES ($1, $2, $3, $4, '{}') RETURNING id`,
		tenantID, projectID, tokenHash[:], keyType).Scan(&keyID))
	return keyID
}

func seedControlPanelUser(t *testing.T, c *cluster, email, password string, platformAdmin bool) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	require.NoError(t, err)
	var id int64
	// Seeded test users are pre-verified — the migration backfill only
	// covers rows that existed at migration time, but the integration
	// suite inserts users after that. Without this, login redirects to
	// /verify and home-page CSRF lookups break.
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO control_panel_users (email, password_hash, is_platform_admin, email_verified_at)
		 VALUES ($1, $2, $3, now()) RETURNING id`,
		email, hash, platformAdmin).Scan(&id))
	if platformAdmin {
		_, err = c.bootstrapPool.Exec(context.Background(),
			`INSERT INTO casbin_rule (ptype, v0, v1, v2)
			 VALUES ('g', $1, $2, '*')
			 ON CONFLICT DO NOTHING`,
			rbac.ControlPanelSubject(id), rbac.RolePlatformAdmin)
		require.NoError(t, err)
	}
	return id
}

func seedControlPanelMembership(t *testing.T, c *cluster, userID, tenantID int64, role string) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role) VALUES ($1, $2, $3)`,
		userID, tenantID, role)
	require.NoError(t, err)
	membershipRole, ok := rbac.ControlPanelMembershipRole(role)
	require.True(t, ok)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO casbin_rule (ptype, v0, v1, v2)
		 VALUES ('g', $1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		rbac.ControlPanelSubject(userID), membershipRole, rbac.TenantDomain(tenantID))
	require.NoError(t, err)
}

func controlPanelLogin(t *testing.T, baseURL, email, password string) *http.Response {
	t.Helper()
	form := url.Values{"email": {email}, "password": {password}}
	resp, err := noRedirectClient().Post(baseURL+"/v1/control-panel/login", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	return resp
}

func controlPanelLoginCookieAndCSRF(t *testing.T, baseURL, email, password string) (*http.Cookie, string) {
	t.Helper()
	resp := controlPanelLogin(t, baseURL, email, password)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/control-panel", nil)
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
