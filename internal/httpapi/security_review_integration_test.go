//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accountSignup drives the global-account signup form (GET for CSRF, then POST)
// and returns the response status + Location header.
func accountSignup(t *testing.T, baseURL, email, password string) (int, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signupURL := baseURL + "/v1/players/account/signup"

	getResp, err := client.Get(signupURL)
	require.NoError(t, err)
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, string(body))
	csrf := extractCSRFFromForm(t, string(body))

	form := url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}}
	req, err := http.NewRequest(http.MethodPost, signupURL, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// TestAccountSignup_duplicate_email_indistinguishable proves the anti-enumeration
// decoy: a signup with an already-registered email returns exactly the same
// response as a fresh signup (redirect to verify), not a distinguishing 409.
func TestAccountSignup_duplicate_email_indistinguishable(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "sk")
	srv, _ := newControlPanelAndPlayerServer(t, c)

	// Fresh signup creates the account and redirects to verify.
	status, loc := accountSignup(t, srv.URL, "dup@example.com", "hunter2hunter2")
	require.Equal(t, http.StatusSeeOther, status)
	require.Contains(t, loc, "/verify")

	// A second signup with the SAME email must be indistinguishable.
	dupStatus, dupLoc := accountSignup(t, srv.URL, "dup@example.com", "hunter2hunter2")
	assert.Equal(t, status, dupStatus, "duplicate signup status must match a fresh one")
	assert.Equal(t, loc, dupLoc, "duplicate signup redirect must match a fresh one")

	// A brand-new email is also the same response — no oracle either way.
	newStatus, newLoc := accountSignup(t, srv.URL, "fresh@example.com", "hunter2hunter2")
	assert.Equal(t, status, newStatus)
	assert.Equal(t, loc, newLoc)
}

// seedProjectWithAPIKey adds a second project + project-pinned api key under an
// existing tenant, so a test can exercise cross-project isolation within one
// tenant.
func seedProjectWithAPIKey(t *testing.T, c *cluster, tenantID int64, token string) int64 {
	t.Helper()
	ctx := context.Background()
	var projectID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
		tenantID, "project-"+token).Scan(&projectID))
	sum := sha256.Sum256([]byte(token))
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO api_keys (tenant_id, project_id, key_hash) VALUES ($1, $2, $3)`,
		tenantID, projectID, sum[:])
	require.NoError(t, err)
	return projectID
}

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

// TestGameSession_cross_project_isolation proves a player authenticated for one
// project cannot read, resolve, or join a session belonging to a different
// project of the SAME tenant (which would leak peers' public IP:port across
// projects).
func TestGameSession_cross_project_isolation(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k1")
	seedProjectWithAPIKey(t, c, tenantID, "k2") // second project, same tenant
	srv := newServerForCluster(t, c)

	// Host in project 1 creates a session.
	tokH, _ := anonymousLoginWithID(t, srv.URL, "k1")
	sess := createSession(t, srv.URL, "k1", tokH, 4)

	// A player in project 2 (same tenant) must not touch project 1's session.
	tokOther, _ := anonymousLoginWithID(t, srv.URL, "k2")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session/%s", srv.URL, sess.SessionID), "k2", tokOther, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project GET: %s", string(body))

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session?joinCode=%s", srv.URL, sess.JoinCode), "k2", tokOther, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project resolve: %s", string(body))

	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k2", tokOther,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project join: %s", string(body))
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
