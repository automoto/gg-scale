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

// ── helpers ─────────────────────────────────────────────────────────────────

func setEndUserEmail(t *testing.T, c *cluster, id int64, email string) {
	t.Helper()
	// bootstrapPool connects as the DB superuser, which bypasses RLS.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE end_users SET email = $1 WHERE id = $2`, email, id)
	require.NoError(t, err)
}

func makeFriends(t *testing.T, baseURL, apiKey string, idA int64, tokA string, idB int64, tokB string) {
	t.Helper()
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", baseURL, idB), apiKey, tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/accept", baseURL, idA), apiKey, tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}

func addr(ip string, port int) map[string]any {
	return map[string]any{"ip": ip, "port": port}
}

type sessionResp struct {
	SessionID string `json:"session_id"`
	JoinCode  string `json:"join_code"`
	State     string `json:"state"`
	Peers     []struct {
		EndUserID int64  `json:"end_user_id"`
		XUID      string `json:"xuid"`
	} `json:"peers"`
}

func createSession(t *testing.T, baseURL, apiKey, token string, maxPlayers int) sessionResp {
	t.Helper()
	resp, body := authedReq(t, http.MethodPost, baseURL+"/v1/game-session", apiKey, token,
		map[string]any{"public_addr": addr("1.2.3.4", 9000), "max_players": maxPlayers})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var out sessionResp
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// ── game session lifecycle ──────────────────────────────────────────────────

func TestGameSession_host_create_sees_self_as_peer(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "k")
	out := createSession(t, srv.URL, "k", tok, 2)

	assert.NotEmpty(t, out.SessionID)
	assert.Len(t, out.JoinCode, 6)
	assert.Equal(t, "open", out.State)
	require.Len(t, out.Peers, 1)
	assert.Equal(t, id, out.Peers[0].EndUserID)
}

func TestGameSession_join_sees_both_peers(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokJ, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokH, 4)

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var joined sessionResp
	require.NoError(t, json.Unmarshal(body, &joined))
	assert.Len(t, joined.Peers, 2)
}

func TestGameSession_resolve_by_join_code(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tok, 2)

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/game-session?joinCode=%s", srv.URL, sess.JoinCode), "k", tok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got struct {
		SessionID string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, sess.SessionID, got.SessionID)
}

func TestGameSession_max_players_enforced(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokJ, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokK, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokH, 2) // host + 1 joiner = full

	resp, _ := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", tokK,
		map[string]any{"public_addr": addr("9.9.9.9", 9002)})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
}

func TestGameSession_host_leave_ends_session(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokJ, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokH, 4)

	resp, _ := authedReq(t, http.MethodDelete,
		fmt.Sprintf("%s/v1/game-session/%s", srv.URL, sess.SessionID), "k", tokH, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	assert.Equal(t, http.StatusGone, resp.StatusCode, string(body))
}

func TestGameSession_heartbeat_non_member_rejected(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokStranger, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokH, 4)

	// Member heartbeat works.
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/heartbeat", srv.URL, sess.SessionID), "k", tokH,
		map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Non-member with a valid session ID must not get the roster.
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/heartbeat", srv.URL, sess.SessionID), "k", tokStranger,
		map[string]any{})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestGameSession_expired_not_joinable(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	ctx := context.Background()
	var hostID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO end_users (tenant_id, project_id, external_id) VALUES ($1,$2,'exp_host') RETURNING id`,
		tenantID, projectID).Scan(&hostID))
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session (id, join_code, tenant_id, project_id, host_user_id, state, props, max_players, expires_at)
		 VALUES ('gs_expired', 'EXP123', $1, $2, $3, 'open', '{}', 4, now() - interval '1 hour')`,
		tenantID, projectID, hostID)
	require.NoError(t, err)

	tokJ, _ := anonymousLoginWithID(t, srv.URL, "k")
	resp, body := authedReq(t, http.MethodPost,
		srv.URL+"/v1/game-session/gs_expired/join", "k", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	assert.Equal(t, http.StatusGone, resp.StatusCode, string(body))
}

func TestGameSession_cap_rejects_overflow(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	ctx := context.Background()
	var hostID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO end_users (tenant_id, project_id, external_id) VALUES ($1,$2,'cap_host') RETURNING id`,
		tenantID, projectID).Scan(&hostID))
	for i := 0; i < 100; i++ {
		_, err := c.bootstrapPool.Exec(ctx,
			`INSERT INTO game_session (id, join_code, tenant_id, project_id, host_user_id, state, props, max_players, expires_at)
			 VALUES ($1, $2, $3, $4, $5, 'open', '{}', 2, now() + interval '4 hours')`,
			fmt.Sprintf("gs_cap_%04d", i), fmt.Sprintf("C%05d", i), tenantID, projectID, hostID)
		require.NoError(t, err)
	}

	tok, _ := anonymousLoginWithID(t, srv.URL, "k")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/game-session", "k", tok,
		map[string]any{"public_addr": addr("1.2.3.4", 9000)})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
}

// ── presence ────────────────────────────────────────────────────────────────

func TestPresence_accepts_custom_status_rejects_empty(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "k")

	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tok,
		map[string]string{"status": "watching_replay"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tok,
		map[string]string{"status": ""})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
}

// ── game invites (by email) ─────────────────────────────────────────────────

func TestGameInvite_requires_friendship(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, _ := anonymousLoginWithID(t, srv.URL, "k")
	_, idB := anonymousLoginWithID(t, srv.URL, "k")
	setEndUserEmail(t, c, idB, "b@example.com")
	sess := createSession(t, srv.URL, "k", tokA, 4)

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/invite", "k", tokA,
		map[string]string{"to_email": "b@example.com", "session_id": sess.SessionID})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestGameInvite_requires_sender_membership(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	tokC, _ := anonymousLoginWithID(t, srv.URL, "k")
	setEndUserEmail(t, c, idB, "b2@example.com")
	makeFriends(t, srv.URL, "k", idA, tokA, idB, tokB)

	// C owns the session; A is friends with B but is NOT in C's session.
	sess := createSession(t, srv.URL, "k", tokC, 4)

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/invite", "k", tokA,
		map[string]string{"to_email": "b2@example.com", "session_id": sess.SessionID})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestGameInvite_happy_path_by_email(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	setEndUserEmail(t, c, idA, "alice@example.com")
	setEndUserEmail(t, c, idB, "bob@example.com")
	makeFriends(t, srv.URL, "k", idA, tokA, idB, tokB)
	sess := createSession(t, srv.URL, "k", tokA, 4)

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/invite", "k", tokA,
		map[string]string{"to_email": "bob@example.com", "session_id": sess.SessionID})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var created struct {
		InviteID int64 `json:"invite_id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	assert.Positive(t, created.InviteID)

	// B sees the invite, with the sender's email.
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/invite", "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var inbox struct {
		Invites []struct {
			InviteID  int64  `json:"invite_id"`
			FromEmail string `json:"from_email"`
			SessionID string `json:"session_id"`
		} `json:"invites"`
	}
	require.NoError(t, json.Unmarshal(body, &inbox))
	require.Len(t, inbox.Invites, 1)
	assert.Equal(t, created.InviteID, inbox.Invites[0].InviteID)
	assert.Equal(t, "alice@example.com", inbox.Invites[0].FromEmail)

	// B dismisses it.
	resp, _ = authedReq(t, http.MethodDelete,
		fmt.Sprintf("%s/v1/invite/%d", srv.URL, created.InviteID), "k", tokB, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/invite", "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &inbox))
	assert.Empty(t, inbox.Invites)
}

func TestGameInvite_unknown_email_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", tokA, 4)

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/invite", "k", tokA,
		map[string]string{"to_email": "nobody@example.com", "session_id": sess.SessionID})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

// ── friends enrichment + profile xuid ───────────────────────────────────────

func TestFriends_enriched_with_email_xuid_presence(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	setEndUserEmail(t, c, idB, "friendb@example.com")
	makeFriends(t, srv.URL, "k", idA, tokA, idB, tokB)

	// B sets an xuid and presence.
	resp, body := authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "k", tokB,
		map[string]string{"xuid": "gamerB"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/presence", "k", tokB,
		map[string]string{"status": "in_match"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// A lists friends — B's email, xuid, and presence appear.
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/friends/", "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var list struct {
		Items []struct {
			Email    *string `json:"email"`
			XUID     *string `json:"xuid"`
			Presence *struct {
				Status string `json:"status"`
			} `json:"presence"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Items, 1)
	require.NotNil(t, list.Items[0].Email)
	assert.Equal(t, "friendb@example.com", *list.Items[0].Email)
	require.NotNil(t, list.Items[0].XUID)
	assert.Equal(t, "gamerB", *list.Items[0].XUID)
	require.NotNil(t, list.Items[0].Presence)
	assert.Equal(t, "in_match", list.Items[0].Presence.Status)
}

func TestFriends_empty_returns_array_not_null(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "k")
	resp, body := authedReq(t, http.MethodGet, srv.URL+"/v1/friends/", "k", tok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"items":[]`)
}

func TestProfile_xuid_uniqueness_conflict(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, _ := anonymousLoginWithID(t, srv.URL, "k")
	tokB, _ := anonymousLoginWithID(t, srv.URL, "k")

	resp, _ := authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "k", tokA,
		map[string]string{"xuid": "dup"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body := authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "k", tokB,
		map[string]string{"xuid": "dup"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
}
