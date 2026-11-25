//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cachedManagementToken inserts a management token that creatorID made for
// the fixture tenant's project, checks it once so the server caches it, and
// returns a func that sends one request with it and gives the status.
func cachedManagementToken(t *testing.T, f keyCacheWriteFixture, creatorID int64) (tokenID, projectID int64, health func() int) {
	t.Helper()
	token := fmt.Sprintf("ggm_cache_drop_%d", creatorID)
	sum := sha256.Sum256([]byte(token))
	require.NoError(t, f.c.bootstrapPool.QueryRow(t.Context(),
		`SELECT id FROM projects WHERE tenant_id = $1`, f.tenantID).Scan(&projectID))
	require.NoError(t, f.c.bootstrapPool.QueryRow(t.Context(),
		`INSERT INTO management_tokens (tenant_id, project_id, created_by_user_id, label, token_hash, token_hint, scopes, expires_at)
		 VALUES ($1, $2, $3, 'test', $4, 'hint', '{}', now() + interval '1 hour') RETURNING id`,
		f.tenantID, projectID, creatorID, sum[:]).Scan(&tokenID))
	health = func() int {
		req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/project/health", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, health(), "the token must work, and be cached, before the write")
	return tokenID, projectID, health
}

func TestManagementTokenCache_should_refuse_a_cached_token_at_once_after_revoke(t *testing.T) {
	f := newKeyCacheWriteFixture(t, "mgmt-revoke-key", false)
	tokenID, projectID, health := cachedManagementToken(t, f, f.userID)
	revoke := fmt.Sprintf("/tenants/%d/projects/%d/management-tokens/%d/revoke", f.tenantID, projectID, tokenID)
	require.Equal(t, http.StatusSeeOther, f.post(revoke, nil))

	status := health()

	assert.Equal(t, http.StatusUnauthorized, status)
}

func TestManagementTokenCache_should_refuse_a_cached_token_at_once_after_its_creator_is_disabled(t *testing.T) {
	// The creator's role must be in the database before the server loads
	// the policy.
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "mgmt-disable-key")
	creatorID := seedControlPanelUser(t, c, "token-creator@example.com", keyCacheOwnerPassword, false)
	seedControlPanelMembership(t, c, creatorID, tenantID, "owner")
	f := newKeyCacheWriteFixtureFor(t, c, tenantID, "mgmt-disable-key", true)
	_, _, health := cachedManagementToken(t, f, creatorID)
	require.Equal(t, http.StatusSeeOther, f.post(fmt.Sprintf("/admin/users/%d/disable", creatorID), nil))

	status := health()

	assert.NotEqual(t, http.StatusOK, status)
}

func TestAllowedOrigins_control_panel_save_should_allow_the_origin_at_once(t *testing.T) {
	f := newKeyCacheWriteFixture(t, "origins-drop-key", false)
	const origin = "https://web-build.example"
	preflight := func() string {
		req, err := http.NewRequest(http.MethodOptions, f.srv.URL+"/v1/config", nil)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Origin")
	}
	require.Empty(t, preflight(), "the empty list must be loaded, and cached, before the save")
	var projectID int64
	require.NoError(t, f.c.bootstrapPool.QueryRow(t.Context(),
		`SELECT id FROM projects WHERE tenant_id = $1`, f.tenantID).Scan(&projectID))
	save := fmt.Sprintf("/tenants/%d/projects/%d/allowed-origins", f.tenantID, projectID)
	require.Equal(t, http.StatusSeeOther, f.post(save, url.Values{"origins": {origin}}))

	got := preflight()

	assert.Equal(t, origin, got)
}
