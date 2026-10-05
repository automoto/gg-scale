//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

// The ban and disable forms change state in either direction, so a request
// without an explicit true/false must not pick a direction for the admin.

func TestControlPanelPlayer_ban_form_without_value_should_keep_ban(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cp-ban")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'banned') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	linkPlayerAccount(t, c, playerID)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	base := srv.URL + "/v1/control-panel" + playerDeleteBase(tenantID, projectID, playerID)
	resp := postForm(t, noRedirectClient(), base+"/ban", url.Values{"_csrf": {csrf}, "ban": {"true"}}, cookie)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	for _, v := range []url.Values{{"_csrf": {csrf}}, {"_csrf": {csrf}, "ban": {"yes"}}} {
		resp := postForm(t, noRedirectClient(), base+"/ban", v, cookie)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	var bans int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM tenant_player_bans WHERE tenant_id = $1`, tenantID).Scan(&bans))
	assert.Equal(t, int64(1), bans)
}

func TestControlPanelPlayer_disable_form_without_value_should_keep_state(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "cp-dis")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'active') RETURNING id`,
		tenantID, projectID).Scan(&playerID))
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	base := srv.URL + "/v1/control-panel" + playerDeleteBase(tenantID, projectID, playerID)

	resp := postForm(t, noRedirectClient(), base+"/disable", url.Values{"_csrf": {csrf}}, cookie)
	resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
