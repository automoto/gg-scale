//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ─────────────────────────────────────────────────────────────────

type browseEntry struct {
	SessionID       string          `json:"session_id"`
	TitleID         string          `json:"title_id"`
	Props           json.RawMessage `json:"props"`
	PlayerCount     int             `json:"player_count"`
	MaxPlayers      int             `json:"max_players"`
	HostPlayerID    int64           `json:"host_player_id"`
	HostDisplayName string          `json:"host_display_name"`
	CreatedAt       time.Time       `json:"created_at"`
}

type browseResult struct {
	Items      []browseEntry `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

func browseSessions(t *testing.T, baseURL, apiKey, token, query string) (browseResult, []byte) {
	t.Helper()
	url := baseURL + "/v1/game-sessions"
	if query != "" {
		url += "?" + query
	}
	resp, body := authedReq(t, http.MethodGet, url, apiKey, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out browseResult
	require.NoError(t, json.Unmarshal(body, &out))
	return out, body
}

func createSessionWith(t *testing.T, baseURL, apiKey, token string, req map[string]any) sessionResp {
	t.Helper()
	if _, ok := req["public_addr"]; !ok {
		req["public_addr"] = addr("1.2.3.4", 9000)
	}
	resp, body := authedReq(t, http.MethodPost, baseURL+"/v1/game-session", apiKey, token, req)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var out sessionResp
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// ── listing ─────────────────────────────────────────────────────────────────

func TestSessionBrowser_lists_public_open_session_then_join_works(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, hostID := linkedPlayerWithName(t, c, srv.URL, "sb", "Hosty")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{
		"title_id":    "arena",
		"props":       map[string]any{"map": "dust"},
		"max_players": 4,
	})

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, body := browseSessions(t, srv.URL, "sb", tokB, "")
	require.Len(t, out.Items, 1, string(body))
	item := out.Items[0]
	assert.Equal(t, sess.SessionID, item.SessionID)
	assert.Equal(t, "arena", item.TitleID)
	assert.JSONEq(t, `{"map":"dust"}`, string(item.Props))
	assert.Equal(t, 1, item.PlayerCount)
	assert.Equal(t, 4, item.MaxPlayers)
	assert.Equal(t, hostID, item.HostPlayerID)
	assert.Equal(t, "Hosty", item.HostDisplayName)
	assert.False(t, item.CreatedAt.IsZero())
	assert.NotContains(t, string(body), "join_code",
		"the browser must not expose join codes")

	// The classic flow: pick a session from the list, join it by id.
	resp, joinBody := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "sb", tokB,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(joinBody))
}

func TestSessionBrowser_empty_returns_array_not_null(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	_, body := browseSessions(t, srv.URL, "sb", tok, "")
	assert.Contains(t, string(body), `"items":[]`)
}

func TestSessionBrowser_host_without_name_omits_host_display_name(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4})

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, body := browseSessions(t, srv.URL, "sb", tokB, "")
	require.Len(t, out.Items, 1)
	assert.NotContains(t, string(body), "host_display_name")
}

// ── exclusions ──────────────────────────────────────────────────────────────

func TestSessionBrowser_private_session_never_listed(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4, "private": true})

	// Not even the host sees a private session in the browser.
	out, _ := browseSessions(t, srv.URL, "sb", tokH, "")
	assert.Empty(t, out.Items)

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ = browseSessions(t, srv.URL, "sb", tokB, "")
	assert.Empty(t, out.Items)
}

func TestSessionBrowser_full_session_not_listed(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	tokJ, _ := anonymousLoginWithID(t, srv.URL, "sb")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 2})
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "sb", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	tokC, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tokC, "")
	assert.Empty(t, out.Items, "a session at max_players must not be listed")
}

func TestSessionBrowser_ended_session_not_listed(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4})
	resp, _ := authedReq(t, http.MethodDelete,
		srv.URL+"/v1/game-session/"+sess.SessionID, "sb", tokH, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tokB, "")
	assert.Empty(t, out.Items)
}

func TestSessionBrowser_expired_session_not_listed(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	ctx := context.Background()
	_, hostID := anonymousLoginWithID(t, srv.URL, "sb")
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session (id, join_code, tenant_id, project_id, host_player_id, state, props, max_players, expires_at)
		 VALUES ('gs_br_exp', 'BREXP1', $1, $2, $3, 'open', '{}', 4, now() - interval '1 minute')`,
		tenantID, projectID, hostID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session_peer (tenant_id, session_id, player_id, ip, port)
		 VALUES ($1, 'gs_br_exp', $2, '1.2.3.4', 9000)`, tenantID, hostID)
	require.NoError(t, err)

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tok, "")
	assert.Empty(t, out.Items, "an expired session must not be listed even with an active peer")
}

func TestSessionBrowser_in_progress_session_not_listed(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4})
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE game_session SET state = 'in_progress' WHERE id = $1`, sess.SessionID)
	require.NoError(t, err)

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tokB, "")
	assert.Empty(t, out.Items, "only open sessions belong in the browser")
}

func TestSessionBrowser_session_with_no_live_peers_not_listed(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4})
	// The host stopped heartbeating 31s ago — the lobby is a ghost.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE game_session_peer SET last_seen = now() - interval '31 seconds' WHERE session_id = $1`,
		sess.SessionID)
	require.NoError(t, err)

	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tokB, "")
	assert.Empty(t, out.Items, "a session with no peer heartbeat in the window must not be listed")
}

func TestSessionBrowser_player_count_tracks_heartbeat_window(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokH, _ := anonymousLoginWithID(t, srv.URL, "sb")
	tokJ, joinerID := anonymousLoginWithID(t, srv.URL, "sb")
	sess := createSessionWith(t, srv.URL, "sb", tokH, map[string]any{"max_players": 4})
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "sb", tokJ,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	tokC, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tokC, "")
	require.Len(t, out.Items, 1)
	require.Equal(t, 2, out.Items[0].PlayerCount)

	// The joiner goes silent → the count drops back to the live host.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE game_session_peer SET last_seen = now() - interval '31 seconds'
		 WHERE session_id = $1 AND player_id = $2`, sess.SessionID, joinerID)
	require.NoError(t, err)

	out, _ = browseSessions(t, srv.URL, "sb", tokC, "")
	require.Len(t, out.Items, 1)
	assert.Equal(t, 1, out.Items[0].PlayerCount)
}

func TestSessionBrowser_cross_project_sessions_not_listed(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)
	ctx := context.Background()

	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'project-b') RETURNING id`,
		tenantID).Scan(&projectB))
	var foreignHost int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'fh') RETURNING id`,
		tenantID, projectB).Scan(&foreignHost))
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session (id, join_code, tenant_id, project_id, host_player_id, state, props, max_players, expires_at)
		 VALUES ('gs_br_foreign', 'BRFOR1', $1, $2, $3, 'open', '{}', 4, now() + interval '1 hour')`,
		tenantID, projectB, foreignHost)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(ctx,
		`INSERT INTO game_session_peer (tenant_id, session_id, player_id, ip, port)
		 VALUES ($1, 'gs_br_foreign', $2, '1.2.3.4', 9000)`, tenantID, foreignHost)
	require.NoError(t, err)

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tok, "")
	assert.Empty(t, out.Items, "sessions of a sibling project must never be listed")
}

// ── filter + pagination ─────────────────────────────────────────────────────

func TestSessionBrowser_title_filter(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	tokA, _ := anonymousLoginWithID(t, srv.URL, "sb")
	tokB, _ := anonymousLoginWithID(t, srv.URL, "sb")
	alpha := createSessionWith(t, srv.URL, "sb", tokA, map[string]any{"title_id": "alpha", "max_players": 4})
	createSessionWith(t, srv.URL, "sb", tokB, map[string]any{"title_id": "beta", "max_players": 4})

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tok, "title_id=alpha")
	require.Len(t, out.Items, 1)
	assert.Equal(t, alpha.SessionID, out.Items[0].SessionID)
}

// TestSessionBrowser_exact_final_page_has_no_cursor: when the result count
// equals the page size, next_cursor must stay empty — a client looping until
// the cursor clears must not make a wasted extra request.
func TestSessionBrowser_exact_final_page_has_no_cursor(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	for range 2 {
		tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
		createSessionWith(t, srv.URL, "sb", tok, map[string]any{"max_players": 4})
	}

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	out, _ := browseSessions(t, srv.URL, "sb", tok, "limit=2")
	require.Len(t, out.Items, 2)
	assert.Empty(t, out.NextCursor)
}

func TestSessionBrowser_pagination_walks_every_session_once(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sb")
	srv := newServerForCluster(t, c)

	want := map[string]bool{}
	for range 3 {
		tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
		want[createSessionWith(t, srv.URL, "sb", tok, map[string]any{"max_players": 4}).SessionID] = true
	}

	tok, _ := anonymousLoginWithID(t, srv.URL, "sb")
	page1, _ := browseSessions(t, srv.URL, "sb", tok, "limit=2")
	require.Len(t, page1.Items, 2)
	require.NotEmpty(t, page1.NextCursor)

	page2, _ := browseSessions(t, srv.URL, "sb", tok, "limit=2&cursor="+page1.NextCursor)
	require.Len(t, page2.Items, 1)
	assert.Empty(t, page2.NextCursor)

	got := map[string]bool{}
	for _, it := range append(page1.Items, page2.Items...) {
		got[it.SessionID] = true
	}
	assert.Equal(t, want, got, "the two pages must cover every session exactly once")
}
