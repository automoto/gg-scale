//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bumpPlayerEpoch simulates a ban/disable/password-change by advancing the
// player's session_epoch out of band (bootstrapPool bypasses RLS).
func bumpPlayerEpoch(t *testing.T, c *cluster, playerID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET session_epoch = session_epoch + 1 WHERE id = $1`, playerID)
	require.NoError(t, err)
}

// TestPlayerAuth_stale_epoch_rejected proves the central epoch check: once a
// player's session_epoch moves past the token's snapshot (a ban bumps it), the
// live JWT is rejected on player routes immediately, not after the token TTL.
func TestPlayerAuth_stale_epoch_rejected(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "k")

	// The token works before the epoch moves.
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tok,
		map[string]string{"status": "online"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Ban-equivalent: the stored epoch advances past the token's snapshot.
	bumpPlayerEpoch(t, c, id)

	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tok,
		map[string]string{"status": "online"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
}

// TestFriends_blockee_cannot_unfriend_away_block proves unfriend never deletes a
// 'blocked' edge: the blockee can't clear the blocker's block by calling
// unfriend, so the block stays in force.
func TestFriends_blockee_cannot_unfriend_away_block(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	makeFriends(t, c, srv.URL, "k", idA, tokA, idB, tokB)

	// A blocks B.
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/block", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// B tries to unfriend A — the block edge is protected, so nothing is
	// deleted (404, indistinguishable from having no relationship).
	resp, body = authedReq(t, http.MethodDelete,
		fmt.Sprintf("%s/v1/friends/%d", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))

	// The block is still in force: B's re-request is refused (indistinguishable
	// from a non-existent target).
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idA), "k", tokB, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

// TestFriends_pending_list_hides_presence proves presence is shared only between
// accepted friends: an outgoing pending request does not leak the target's live
// presence.
func TestFriends_pending_list_hides_presence(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, idA)
	linkPlayerAccount(t, c, idB)

	// A sends B a friend request (pending, not accepted).
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// B is online.
	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tokB,
		map[string]string{"status": "in_match"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// A's pending list includes B but must not carry B's presence.
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/friends/?status=pending", "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var list struct {
		Items []struct {
			PlayerID *int64 `json:"player_id"`
			Presence *struct {
				Status string `json:"status"`
			} `json:"presence"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Items, 1)
	assert.Nil(t, list.Items[0].Presence, "pending friend list must not leak presence")
}

// TestGameSession_get_roster_requires_membership proves a non-member cannot read
// a session's peer roster (public IP:port) — only the host/members can.
func TestGameSession_get_roster_requires_membership(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokStranger, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokH, 4)

	// Host (a member) can read the roster.
	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session/%s", srv.URL, sess.SessionID), "k", tokH, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// A stranger with the session id gets nothing — no roster, no existence leak.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session/%s", srv.URL, sess.SessionID), "k", tokStranger, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

// TestGameSession_private_not_discoverable proves a private session can't be
// resolved by join code or joined by a non-member/non-invitee.
func TestGameSession_private_not_discoverable(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokStranger, _ := anonymousLoginWithID(t, srv.URL, "k")

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/game-session", "k", tokH,
		map[string]any{"public_addr": addr("1.2.3.4", 9000), "max_players": 4, "private": true})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var sess sessionResp
	require.NoError(t, json.Unmarshal(body, &sess))

	// Host resolves their own private session by code.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session?joinCode=%s", srv.URL, sess.JoinCode), "k", tokH, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// A stranger cannot resolve it by code.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session?joinCode=%s", srv.URL, sess.JoinCode), "k", tokStranger, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))

	// A stranger cannot join it even with the session id.
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", tokStranger,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}
