//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

const (
	keyCacheOwnerPassword    = "correct-horse-battery-staple"
	keyCacheEntitlementToken = "entitlement-token-0123456789abcdef"
)

// newKeyCacheServer builds the API, the management API, and the control
// panel on one set of caches, as the server does. The key, grant, management
// token, and origin cache times are long, so only a drop on write can change
// a cached result. lookups counts the key reads that reach the database.
func newKeyCacheServer(t *testing.T, c *cluster) (srv *httptest.Server, lookups *atomic.Int64, authorizer *rbac.Authorizer) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err = rbac.NewAuthorizerWithOptions(pool, rbac.Options{FeatureGrantCacheTTL: time.Hour})
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	lookups = &atomic.Int64{}
	inner := tenant.NewSQLLookup(c.appPool)
	keys := tenant.NewKeyCache(func(ctx context.Context, hash []byte) (*tenant.APIKey, error) {
		lookups.Add(1)
		return inner(ctx, hash)
	}, time.Hour, 100)
	srv = httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version:                        "v1",
		Commit:                         "test",
		Pool:                           pool,
		Lookup:                         keys.Lookup,
		APIKeyCache:                    keys,
		Limiter:                        ratelimit.NewCacheLimiter(c.cache),
		Signer:                         signer,
		Cache:                          c.cache,
		RBAC:                           authorizer,
		Mailer:                         &mailer.Recorder{},
		EmailVerifySigningKey:          []byte(testEmailVerifySigningKey),
		ControlPanel:                   controlpanel.Config{Mount: true, MailFrom: "noreply@ggscale.test", ManagementAPIEnabled: true},
		ControlPanelBootstrap:          controlpanel.DisabledBootstrap(),
		EntitlementAPIToken:            keyCacheEntitlementToken,
		CORSAllowedOrigins:             []string{"https://app.example"},
		ManagementAPIEnabled:           true,
		ManagementTokenRatePerSecond:   1000,
		ManagementTokenBurst:           1000,
		ManagementTokenCacheTTL:        time.Hour,
		ManagementTokenCacheMaxEntries: 100,
	}))
	t.Cleanup(srv.Close)
	return srv, lookups, authorizer
}

// keyStatus sends one API request with the key and returns the status.
func keyStatus(t *testing.T, baseURL, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/config", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// keyCacheFixture seeds a tenant with one key and an owner, and returns the
// server with the key already in the cache. post sends a control panel form
// as the owner; its path starts at the tenant.
func keyCacheFixture(t *testing.T, token string) (c *cluster, srv *httptest.Server, tenantID int64, post func(path string) int) {
	t.Helper()
	f := newKeyCacheWriteFixture(t, token, false)
	post = func(path string) int {
		return f.post(fmt.Sprintf("/tenants/%d%s", f.tenantID, path), nil)
	}
	return f.c, f.srv, f.tenantID, post
}

type keyCacheWriteFixture struct {
	c        *cluster
	srv      *httptest.Server
	lookups  *atomic.Int64
	tenantID int64
	userID   int64
	// post sends a control panel form as the fixture user and returns the
	// status. path starts after /v1/control-panel.
	post func(path string, form url.Values) int
}

// newKeyCacheWriteFixture seeds a tenant with one key and one user that owns
// the tenant, and returns the server with the key already in the cache.
func newKeyCacheWriteFixture(t *testing.T, token string, platformAdmin bool) keyCacheWriteFixture {
	t.Helper()
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, token)
	return newKeyCacheWriteFixtureFor(t, c, tenantID, token, platformAdmin)
}

// newKeyCacheWriteFixtureFor is newKeyCacheWriteFixture for a tenant and key
// that the test seeded.
func newKeyCacheWriteFixtureFor(t *testing.T, c *cluster, tenantID int64, token string, platformAdmin bool) keyCacheWriteFixture {
	t.Helper()
	email := token + "@example.com"
	userID := seedControlPanelUser(t, c, email, keyCacheOwnerPassword, platformAdmin)
	seedControlPanelMembership(t, c, userID, tenantID, "owner")
	srv, lookups, _ := newKeyCacheServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, email, keyCacheOwnerPassword)
	require.Equal(t, http.StatusOK, keyStatus(t, srv.URL, token), "the key must work, and be cached, before the write")

	post := func(path string, form url.Values) int {
		if form == nil {
			form = url.Values{}
		}
		form.Set("_csrf", csrf)
		resp := postForm(t, noRedirectClient(), srv.URL+"/v1/control-panel"+path, form, cookie)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	return keyCacheWriteFixture{c: c, srv: srv, lookups: lookups, tenantID: tenantID, userID: userID, post: post}
}

func TestAPIKeyCache_should_refuse_a_cached_key_at_once_after_revoke(t *testing.T) {
	const token = "cached-revoke-key"
	c, srv, tenantID, post := keyCacheFixture(t, token)
	var keyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(t.Context(),
		`SELECT id FROM api_keys WHERE tenant_id = $1`, tenantID).Scan(&keyID))

	require.Equal(t, http.StatusSeeOther, post(fmt.Sprintf("/api-keys/%d/revoke", keyID)))

	assert.Equal(t, http.StatusForbidden, keyStatus(t, srv.URL, token))
}

func TestAPIKeyCache_should_refuse_a_cached_key_at_once_after_tenant_disable(t *testing.T) {
	const token = "cached-disable-key"
	_, srv, _, post := keyCacheFixture(t, token)

	require.Equal(t, http.StatusSeeOther, post("/settings/disable"))

	assert.Equal(t, http.StatusForbidden, keyStatus(t, srv.URL, token))
}

func TestAPIKeyCache_should_admit_a_cached_key_at_once_after_tenant_enable(t *testing.T) {
	const token = "cached-enable-key"
	_, srv, _, post := keyCacheFixture(t, token)
	require.Equal(t, http.StatusSeeOther, post("/settings/disable"))
	require.Equal(t, http.StatusForbidden, keyStatus(t, srv.URL, token))

	require.Equal(t, http.StatusSeeOther, post("/settings/enable"))

	assert.Equal(t, http.StatusOK, keyStatus(t, srv.URL, token))
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_a_tier_change(t *testing.T) {
	const token = "cached-tier-key"
	f := newKeyCacheWriteFixture(t, token, true)

	require.Equal(t, http.StatusSeeOther,
		f.post(fmt.Sprintf("/tenants/%d/settings/tier", f.tenantID), url.Values{"tier": {"1"}}))
	keyStatus(t, f.srv.URL, token)

	assert.Equal(t, int64(2), f.lookups.Load())
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_a_connection_limit_change(t *testing.T) {
	const token = "cached-connection-key"
	f := newKeyCacheWriteFixture(t, token, true)

	require.Equal(t, http.StatusSeeOther, f.post(fmt.Sprintf("/tenants/%d/rate-limits/connections", f.tenantID),
		url.Values{"sustained": {"250000"}, "ceiling": {"500000"}}))
	keyStatus(t, f.srv.URL, token)

	assert.Equal(t, int64(2), f.lookups.Load())
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_a_scope_change(t *testing.T) {
	const token = "cached-scope-key"
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, token)
	var keyID int64
	require.NoError(t, c.bootstrapPool.QueryRow(t.Context(),
		`UPDATE api_keys SET scopes = '{matchmaker}' WHERE tenant_id = $1 RETURNING id`, tenantID).Scan(&keyID))
	f := newKeyCacheWriteFixtureFor(t, c, tenantID, token, false)

	// A form with no scopes removes the matchmaker scope.
	status := f.post(fmt.Sprintf("/tenants/%d/api-keys/%d/features", tenantID, keyID), nil)
	require.Less(t, status, http.StatusBadRequest)
	keyStatus(t, f.srv.URL, token)

	assert.Equal(t, int64(2), f.lookups.Load())
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_a_change_request_approve(t *testing.T) {
	const token = "cached-approve-key"
	f := newKeyCacheWriteFixture(t, token, true)
	require.Equal(t, http.StatusSeeOther, f.post(fmt.Sprintf("/tenants/%d/settings/change-requests", f.tenantID),
		url.Values{"kind": {"tier_upgrade"}, "requested_tier": {"1"}}))
	var requestID int64
	require.NoError(t, f.c.bootstrapPool.QueryRow(t.Context(),
		`SELECT id FROM tenant_change_requests WHERE tenant_id = $1 AND status = 'pending'`, f.tenantID).Scan(&requestID))

	require.Equal(t, http.StatusSeeOther, f.post(fmt.Sprintf("/admin/change-requests/%d/approve", requestID), nil))
	keyStatus(t, f.srv.URL, token)

	assert.Equal(t, int64(2), f.lookups.Load())
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_an_entitlement_change(t *testing.T) {
	const token = "cached-entitlement-key"
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, token)
	srv, lookups, _ := newKeyCacheServer(t, c)
	require.Equal(t, http.StatusOK, keyStatus(t, srv.URL, token))

	require.Equal(t, http.StatusOK, putEntitlements(t, srv.URL, tenantID, `{"tier":1,"features":[]}`))
	keyStatus(t, srv.URL, token)

	assert.Equal(t, int64(2), lookups.Load())
}

func TestFeatureGrantCache_should_use_a_grant_at_once_after_an_entitlement_change(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "entitlement-grant-key")
	srv, _, authorizer := newKeyCacheServer(t, c)
	// This read caches "not enabled" in the authorizer.
	before, err := authorizer.FeatureEnabled(t.Context(), tenantID, 0, rbac.FeatureP2PRelay)
	require.NoError(t, err)
	require.False(t, before)

	require.Equal(t, http.StatusOK, putEntitlements(t, srv.URL, tenantID, `{"tier":0,"features":["p2p_relay"]}`))

	after, err := authorizer.FeatureEnabled(t.Context(), tenantID, 0, rbac.FeatureP2PRelay)
	require.NoError(t, err)
	assert.True(t, after)
}

// putEntitlements applies an entitlement state as the billing service does.
func putEntitlements(t *testing.T, baseURL string, tenantID int64, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/internal/entitlements/%d", baseURL, tenantID), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+keyCacheEntitlementToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}
