//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

// Admin-side per-project player deletion: tenant admins (and platform admins,
// via the same casbin policy) request and cancel deletions from the control
// panel. A pending deletion owns the disabled state — the plain enable action
// answers 409 until the deletion is cancelled.

func playerDeleteBase(tenantID, projectID, playerID int64) string {
	return fmt.Sprintf("/tenants/%d/projects/%d/players/%d", tenantID, projectID, playerID)
}

func TestControlPanelPlayerDelete_admin_request_and_cancel(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cp-del")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'doomed') RETURNING id`,
		tenantID, projectID).Scan(&playerID))

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	base := srv.URL + "/v1/control-panel" + playerDeleteBase(tenantID, projectID, playerID)

	resp := postForm(t, noRedirectClient(), base+"/request-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var disabledMatches, pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at = delete_requested_at, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, playerID).Scan(&disabledMatches, &pending))
	assert.True(t, pending)
	assert.True(t, disabledMatches)

	var requestAudits int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'control_panel.player.delete_request' AND actor_user_id = $1`, adminID).
		Scan(&requestAudits))
	assert.Equal(t, int64(1), requestAudits)

	// A second request is a conflict, not a fresh timer.
	resp = postForm(t, noRedirectClient(), base+"/request-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	// The pending deletion owns the disabled state: plain enable must refuse.
	resp = postForm(t, noRedirectClient(), base+"/disable",
		url.Values{"_csrf": {csrf}, "enable": {"true"}}, cookie)
	resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	// The detail page renders the pending state with the cancel action.
	req, err := http.NewRequest(http.MethodGet, base, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	pageResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	pageBody, err := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, pageResp.StatusCode)
	assert.Contains(t, string(pageBody), "Deletion scheduled")
	assert.Contains(t, string(pageBody), "cancel-delete")

	resp = postForm(t, noRedirectClient(), base+"/cancel-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var disabled bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at IS NOT NULL, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, playerID).Scan(&disabled, &pending))
	assert.False(t, disabled, "cancel lifts the disable the request created")
	assert.False(t, pending)

	var cancelAudits int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM platform_audit_log
		 WHERE action = 'control_panel.player.delete_cancel' AND actor_user_id = $1`, adminID).
		Scan(&cancelAudits))
	assert.Equal(t, int64(1), cancelAudits)

	// Nothing left to cancel.
	resp = postForm(t, noRedirectClient(), base+"/cancel-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestControlPanelPlayerDelete_preserves_prior_suspension_on_cancel(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cp-del")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id, disabled_at)
		 VALUES ($1, $2, 'suspended', now() - interval '1 hour') RETURNING id`,
		tenantID, projectID).Scan(&playerID))

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	base := srv.URL + "/v1/control-panel" + playerDeleteBase(tenantID, projectID, playerID)

	resp := postForm(t, noRedirectClient(), base+"/request-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var keptSuspension bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at < delete_requested_at FROM project_players WHERE id = $1`, playerID).
		Scan(&keptSuspension))
	assert.True(t, keptSuspension, "the request must keep the earlier suspension timestamp")

	resp = postForm(t, noRedirectClient(), base+"/cancel-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var stillSuspended, pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at IS NOT NULL, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, playerID).Scan(&stillSuspended, &pending))
	assert.True(t, stillSuspended, "cancel must leave the pre-existing suspension in place")
	assert.False(t, pending)
}

func TestControlPanelPlayerDelete_platform_admin_allowed_member_denied(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cp-del")
	seedControlPanelUser(t, c, "platform@example.com", "correct-horse-battery-staple", true)
	memberID := seedControlPanelUser(t, c, "member@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, memberID, tenantID, "member")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'target') RETURNING id`,
		tenantID, projectID).Scan(&playerID))

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	base := srv.URL + "/v1/control-panel" + playerDeleteBase(tenantID, projectID, playerID)

	memberCookie, memberCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "member@example.com", "correct-horse-battery-staple")
	for _, path := range []string{"/request-delete", "/cancel-delete"} {
		resp := postForm(t, noRedirectClient(), base+path, url.Values{"_csrf": {memberCSRF}}, memberCookie)
		resp.Body.Close()
		assert.Equalf(t, http.StatusForbidden, resp.StatusCode, "member must be denied: %s", path)
	}

	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "platform@example.com", "correct-horse-battery-staple")
	resp := postForm(t, noRedirectClient(), base+"/request-delete", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT delete_requested_at IS NOT NULL FROM project_players WHERE id = $1`, playerID).Scan(&pending))
	assert.True(t, pending, "a platform admin can request deletion in any tenant")
}
