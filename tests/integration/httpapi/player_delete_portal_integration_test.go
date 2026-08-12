//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

// The account portal is where a player with a linked global account requests
// per-project data deletion (and cancels it): the flow picks a project from
// the linked-games list, confirms on a dedicated page, and shows the pending
// state with a cancel action until the purge runs.

func TestPlayerPortalDelete_request_and_cancel_round_trip(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	link := sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "gone@example.com")
	acceptPlayerInvite(t, srv, link, "accountpass1")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT id FROM project_players WHERE email = 'gone@example.com'`).Scan(&playerID))

	account := playerAccountClient(t, srv.URL, "gone@example.com", "accountpass1")
	deletePath := "/v1/players/account/projects/" + strconv.FormatInt(playerID, 10) + "/delete"

	status, home := getPage(t, account, srv.URL+"/v1/players/account/")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, home, deletePath, "the linked-games list offers the delete action")

	status, confirm := getPage(t, account, srv.URL+deletePath)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, confirm, "Delete")
	accountCSRF := extractCSRFFromForm(t, confirm)

	// A foreign player id is not deletable — 404, nothing leaks.
	status, _ = postAccountForm(t, account, srv.URL+"/v1/players/account/projects/999999/delete",
		map[string][]string{"_csrf": {accountCSRF}})
	assert.Equal(t, http.StatusNotFound, status)

	status, _ = postAccountForm(t, account, srv.URL+deletePath,
		map[string][]string{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)

	var disabledMatches, pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at = delete_requested_at, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, playerID).Scan(&disabledMatches, &pending))
	assert.True(t, pending)
	assert.True(t, disabledMatches, "the request must own the disable so cancel can lift it")

	// Pending state renders with a cancel action instead of a second delete.
	status, home = getPage(t, account, srv.URL+"/v1/players/account/")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, home, deletePath+"/cancel")

	// A stale unlink form must not sever the account link while the deletion
	// is pending — unlinking would remove the portal's cancellation path.
	unlinkPath := "/v1/players/account/projects/" + strconv.FormatInt(playerID, 10) + "/unlink"
	status, _ = postAccountForm(t, account, srv.URL+unlinkPath,
		map[string][]string{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)
	var stillLinked bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT player_account_id IS NOT NULL FROM project_players WHERE id = $1`, playerID).
		Scan(&stillLinked))
	assert.True(t, stillLinked, "unlink must be refused while a deletion is pending")

	status, _ = postAccountForm(t, account, srv.URL+deletePath+"/cancel",
		map[string][]string{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)

	var disabled bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at IS NOT NULL, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, playerID).Scan(&disabled, &pending))
	assert.False(t, disabled, "cancel lifts the disable the request created")
	assert.False(t, pending)

	var requests, cancels int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'account.player.delete_request' AND actor_user_id = $1`,
		playerID).Scan(&requests))
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'account.player.delete_cancel' AND actor_user_id = $1`,
		playerID).Scan(&cancels))
	assert.Equal(t, int64(1), requests)
	assert.Equal(t, int64(1), cancels)

	// Nothing left to cancel: benign redirect, state unchanged.
	status, _ = postAccountForm(t, account, srv.URL+deletePath+"/cancel",
		map[string][]string{"_csrf": {accountCSRF}})
	assert.Equal(t, http.StatusSeeOther, status)
}

func TestPlayerPortalDelete_preserves_prior_admin_suspension(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	link := sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "susp@example.com")
	acceptPlayerInvite(t, srv, link, "accountpass1")
	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT id FROM project_players WHERE email = 'susp@example.com'`).Scan(&playerID))

	// An admin suspended the player an hour before the delete request.
	suspendedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_, err := c.bootstrapPool.Exec(ctx,
		`UPDATE project_players SET disabled_at = $1 WHERE id = $2`, suspendedAt, playerID)
	require.NoError(t, err)

	account := playerAccountClient(t, srv.URL, "susp@example.com", "accountpass1")
	deletePath := "/v1/players/account/projects/" + strconv.FormatInt(playerID, 10) + "/delete"
	status, confirm := getPage(t, account, srv.URL+deletePath)
	require.Equal(t, http.StatusOK, status)
	accountCSRF := extractCSRFFromForm(t, confirm)

	status, _ = postAccountForm(t, account, srv.URL+deletePath,
		map[string][]string{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)

	var keptSuspension bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at = $1 FROM project_players WHERE id = $2`, suspendedAt, playerID).
		Scan(&keptSuspension))
	assert.True(t, keptSuspension, "the request must not overwrite the admin's suspension timestamp")

	status, _ = postAccountForm(t, account, srv.URL+deletePath+"/cancel",
		map[string][]string{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)

	var stillSuspended, pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT disabled_at = $1, delete_requested_at IS NOT NULL FROM project_players WHERE id = $2`,
		suspendedAt, playerID).Scan(&stillSuspended, &pending))
	assert.False(t, pending)
	assert.True(t, stillSuspended, "cancel must leave the pre-existing suspension in place")
}
