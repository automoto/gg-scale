//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/verifycode"
)

// raceAgainstCommit runs act while an open transaction holds the row lock taken
// by lockSQL, waits until act's write to table is blocked on that lock, and then
// commits. The write therefore always follows a read that saw the old row, which
// is the interleaving a guarded UPDATE must refuse. It returns act's status.
func raceAgainstCommit(t *testing.T, c *cluster, table, lockSQL string, act func() int) int {
	t.Helper()
	ctx := context.Background()
	tx, err := c.bootstrapPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, lockSQL)
	require.NoError(t, err)

	status := make(chan int, 1)
	go func() {
		defer close(status)
		status <- act()
	}()

	require.Eventually(t, func() bool {
		var waiting int
		err := c.bootstrapPool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND query ILIKE '%UPDATE ' || $1 || '%'`, table).Scan(&waiting)
		return err == nil && waiting > 0
	}, 30*time.Second, 20*time.Millisecond, "write to %s never blocked on the row lock", table)

	require.NoError(t, tx.Commit(ctx))
	return <-status
}

func friendEdgeStatus(t *testing.T, c *cluster) string {
	t.Helper()
	var status string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT status FROM friend_edges`).Scan(&status))
	return status
}

const blockFriendEdgeSQL = `UPDATE friend_edges SET status = 'blocked', updated_at = now()`

func TestFriendAccept_should_keep_block_when_block_commits_during_accept(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv := newServerForCluster(t, c)
	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, idA)
	linkPlayerAccount(t, c, idB)
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	status := raceAgainstCommit(t, c, "friend_edges", blockFriendEdgeSQL, func() int {
		resp, _ := authedReq(t, http.MethodPost,
			fmt.Sprintf("%s/v1/friends/%d/accept", srv.URL, idA), "k", tokB, nil)
		return resp.StatusCode
	})

	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "blocked", friendEdgeStatus(t, c))
}

// A block never reveals itself: the blockee gets the same 404 as with no edge.
func TestFriendAction_should_hide_block_from_blockee(t *testing.T) {
	for _, action := range []string{"accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			c := startCluster(t)
			seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
			srv := newServerForCluster(t, c)
			tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
			tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
			linkPlayerAccount(t, c, idA)
			linkPlayerAccount(t, c, idB)
			noEdge, noEdgeBody := authedReq(t, http.MethodPost,
				fmt.Sprintf("%s/v1/friends/%d/%s", srv.URL, idA, action), "k", tokB, nil)
			require.Equal(t, http.StatusNotFound, noEdge.StatusCode, string(noEdgeBody))
			resp, body := authedReq(t, http.MethodPost,
				fmt.Sprintf("%s/v1/friends/%d/block", srv.URL, idB), "k", tokA, nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

			blocked, _ := authedReq(t, http.MethodPost,
				fmt.Sprintf("%s/v1/friends/%d/%s", srv.URL, idA, action), "k", tokB, nil)

			assert.Equal(t, http.StatusNotFound, blocked.StatusCode)
		})
	}
}

func TestFriendReject_should_refuse_accepted_edge(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv := newServerForCluster(t, c)
	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	makeFriends(t, c, srv.URL, "k", idA, tokA, idB, tokB)

	resp, _ := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/reject", srv.URL, idA), "k", tokB, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "accepted", friendEdgeStatus(t, c))
}

func TestPortalFriendAction_should_keep_block_when_block_commits_during_action(t *testing.T) {
	for _, action := range []string{"accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			c := startCluster(t)
			srv, _ := newControlPanelAndPlayerServer(t, c)
			base := srv.URL + "/v1/players/account"
			const password = "correct-horse-battery-staple"
			seedVerifiedPlayerAccount(t, c, "me@example.com", password)
			seedVerifiedPlayerAccount(t, c, "other@example.com", password)
			var otherID string
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`INSERT INTO friend_edges (from_account_id, to_account_id, status)
				 SELECT o.id, m.id, 'pending' FROM player_accounts o, player_accounts m
				 WHERE o.email = 'other@example.com' AND m.email = 'me@example.com'
				 RETURNING from_account_id::text`).Scan(&otherID))
			client := loginPlayerAccount(t, base, "me@example.com", password)
			csrf := getPlayerCSRF(t, client, base+"/friends")

			status := raceAgainstCommit(t, c, "friend_edges", blockFriendEdgeSQL, func() int {
				resp, err := client.PostForm(base+"/friends/"+otherID+"/"+action, url.Values{"_csrf": {csrf}})
				require.NoError(t, err)
				resp.Body.Close()
				return resp.StatusCode
			})

			require.Equal(t, http.StatusSeeOther, status)
			assert.Equal(t, "blocked", friendEdgeStatus(t, c))
		})
	}
}

const revokePlayerInvitesSQL = `UPDATE player_invitations SET revoked_at = now()`

func TestPlayerInvite_accept_should_fail_when_revoke_commits_during_accept(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	mail := sendPlayerInvite(t, srv, rec, cookie, csrf, tenantA, projectA, "invitee@example.com")

	status := raceAgainstCommit(t, c, "player_invitations", revokePlayerInvitesSQL, func() int {
		code, _ := acceptInvite(t, srv.URL, mail)
		return code
	})

	assert.Equal(t, http.StatusNotFound, status)
	var players int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE email = 'invitee@example.com'`).Scan(&players))
	assert.Zero(t, players)
}

func TestLinkPlayer_accept_should_fail_when_revoke_commits_during_accept(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-race")
	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")
	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "linked@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	status := raceAgainstCommit(t, c, "player_invitations", revokePlayerInvitesSQL, func() int {
		code, _ := acceptInvite(t, srv.URL, rec.Sent[0].Body)
		return code
	})

	assert.Equal(t, http.StatusNotFound, status)
	var account *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT player_account_id::text FROM project_players WHERE id = $1`, playerID).Scan(&account))
	assert.Nil(t, account)
}

func TestControlPanelInvite_accept_should_fail_when_revoke_commits_during_accept(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	const code = "raw-test-invite-code-very-long-for-security"
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO control_panel_invitations (email, tenant_id, role, code_hash, expires_at, invited_by_user_id)
		 VALUES ('invitee@example.com', $1, 'tenant_admin', $2, now() + interval '1 day', $3)`,
		tenantID, verifycode.Hash(nil, code), adminID)
	require.NoError(t, err)
	client := jarClient(t)
	acceptURL := srv.URL + "/v1/control-panel/invite/accept"
	csrf := getPlayerCSRF(t, client, acceptURL+"?code="+url.QueryEscape(code))

	status := raceAgainstCommit(t, c, "control_panel_invitations",
		`UPDATE control_panel_invitations SET revoked_at = now()`, func() int {
			resp := postForm(t, client, acceptURL, url.Values{
				"_csrf": {csrf}, "code": {code}, "password": {"correct-horse-battery-staple"},
			}, nil)
			resp.Body.Close()
			return resp.StatusCode
		})

	assert.Equal(t, http.StatusNotFound, status)
	var memberships int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM control_panel_memberships WHERE tenant_id = $1`, tenantID).Scan(&memberships))
	assert.Zero(t, memberships)
}
