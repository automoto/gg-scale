//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ─────────────────────────────────────────────────────────────────

type profileBody struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

func getProfile(t *testing.T, baseURL, apiKey, token string) profileBody {
	t.Helper()
	resp, body := authedReq(t, http.MethodGet, baseURL+"/v1/profile", apiKey, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out profileBody
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func patchDisplayName(t *testing.T, baseURL, apiKey, token, name string) (*http.Response, []byte) {
	t.Helper()
	return authedReq(t, http.MethodPatch, baseURL+"/v1/profile", apiKey, token,
		map[string]string{"display_name": name})
}

// linkedPlayerWithName logs in a fresh anonymous player, links it to a global
// account, and sets its display name through the real PATCH endpoint.
func linkedPlayerWithName(t *testing.T, c *cluster, baseURL, apiKey, name string) (string, int64) {
	t.Helper()
	tok, id := anonymousLoginWithID(t, baseURL, apiKey)
	linkPlayerAccount(t, c, id)
	resp, body := patchDisplayName(t, baseURL, apiKey, tok, name)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
	return tok, id
}

type publicPlayerBody struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// ── profile display_name (read/write) ───────────────────────────────────────

func TestProfile_display_name_set_and_read_back(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "dn")
	linkPlayerAccount(t, c, id)

	resp, body := patchDisplayName(t, srv.URL, "dn", tok, "Nova Fox")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	assert.Equal(t, "Nova Fox", getProfile(t, srv.URL, "dn", tok).DisplayName)
}

func TestProfile_display_name_requires_linked_account(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "dn") // unlinked / anonymous
	resp, body := patchDisplayName(t, srv.URL, "dn", tok, "Ghost")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestProfile_display_name_invalid_values_rejected(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "dn")
	linkPlayerAccount(t, c, id)

	cases := []struct {
		name  string
		value string
	}{
		{"too_long", strings.Repeat("x", 65)},
		{"control_char", "bad\aname"},
		{"newline", "line\nbreak"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := patchDisplayName(t, srv.URL, "dn", tok, tc.value)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		})
	}
}

func TestProfile_display_name_empty_string_clears_it(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, _ := linkedPlayerWithName(t, c, srv.URL, "dn", "Fleeting")
	require.Equal(t, "Fleeting", getProfile(t, srv.URL, "dn", tok).DisplayName)

	resp, body := patchDisplayName(t, srv.URL, "dn", tok, "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
	assert.Empty(t, getProfile(t, srv.URL, "dn", tok).DisplayName)
}

// ── public player lookup ────────────────────────────────────────────────────

func TestPlayers_get_returns_public_profile_without_email(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)

	_, target := linkedPlayerWithName(t, c, srv.URL, "pl", "Target Player")
	callerTok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/players/%d", srv.URL, target), "pl", callerTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var out publicPlayerBody
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, target, out.ID)
	assert.Equal(t, "Target Player", out.DisplayName)
	assert.NotEmpty(t, out.CreatedAt)
	// The linked account row carries an email; it must never leak here.
	assert.NotContains(t, string(body), "email")
}

func TestPlayers_get_player_without_name_omits_display_name(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)

	_, target := anonymousLoginWithID(t, srv.URL, "pl") // no account, no name
	callerTok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/players/%d", srv.URL, target), "pl", callerTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.NotContains(t, string(body), "display_name")
}

func TestPlayers_get_cross_project_id_is_404(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	ctx := context.Background()

	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'project-b') RETURNING id`,
		tenantID).Scan(&projectB))
	var victimID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'victim') RETURNING id`,
		tenantID, projectB).Scan(&victimID))

	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/players/%d", srv.URL, victimID), "pl", tok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestPlayers_get_cross_tenant_id_is_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "ta")
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "tb")
	srv := newServerForCluster(t, c)

	_, victimID := anonymousLoginWithID(t, srv.URL, "tb")
	tok, _ := anonymousLoginWithID(t, srv.URL, "ta")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/players/%d", srv.URL, victimID), "ta", tok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestPlayers_get_unknown_id_is_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	resp, body := authedReq(t, http.MethodGet, srv.URL+"/v1/players/999999999", "pl", tok, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

// ── batch resolve ───────────────────────────────────────────────────────────

func TestPlayers_batch_resolves_known_ids_and_omits_the_rest(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)
	ctx := context.Background()

	_, named := linkedPlayerWithName(t, c, srv.URL, "pl", "Named One")
	_, unnamed := anonymousLoginWithID(t, srv.URL, "pl")

	// A player in a sibling project must be silently omitted.
	var projectB, foreignID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'project-b') RETURNING id`,
		tenantID).Scan(&projectB))
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'foreign') RETURNING id`,
		tenantID, projectB).Scan(&foreignID))

	callerTok, _ := anonymousLoginWithID(t, srv.URL, "pl")
	url := fmt.Sprintf("%s/v1/players?ids=%d,%d,%d,999999999", srv.URL, named, unnamed, foreignID)
	resp, body := authedReq(t, http.MethodGet, url, "pl", callerTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var out struct {
		Players []publicPlayerBody `json:"players"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out.Players, 2)
	assert.Equal(t, named, out.Players[0].ID) // results ordered by id
	assert.Equal(t, "Named One", out.Players[0].DisplayName)
	assert.Equal(t, unnamed, out.Players[1].ID)
	assert.Empty(t, out.Players[1].DisplayName)
	assert.NotContains(t, string(body), "email")
}

func TestPlayers_batch_over_cap_is_validation_error(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	ids := make([]string, 101)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	resp, body := authedReq(t, http.MethodGet,
		srv.URL+"/v1/players?ids="+strings.Join(ids, ","), "pl", tok, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
}

// TestPlayers_batch_tolerates_empty_segments covers programmatically built
// lists: a trailing or doubled comma must not fail the whole batch.
func TestPlayers_batch_tolerates_empty_segments(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)

	_, named := linkedPlayerWithName(t, c, srv.URL, "pl", "Comma Fan")
	tok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	for _, ids := range []string{
		fmt.Sprintf("%d,", named),
		fmt.Sprintf(",%d,,", named),
	} {
		resp, body := authedReq(t, http.MethodGet,
			srv.URL+"/v1/players?ids="+ids, "pl", tok, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "ids=%q: %s", ids, string(body))
		var out struct {
			Players []publicPlayerBody `json:"players"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		require.Len(t, out.Players, 1, "ids=%q", ids)
		assert.Equal(t, named, out.Players[0].ID)
	}
}

func TestPlayers_batch_bad_ids_param_is_validation_error(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "pl")
	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "pl")

	cases := []struct {
		name string
		ids  string
	}{
		{"missing", ""},
		{"not_a_number", "abc"},
		{"only_commas", ",,"},
		{"negative", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := authedReq(t, http.MethodGet,
				srv.URL+"/v1/players?ids="+tc.ids, "pl", tok, nil)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		})
	}
}

// TestProfile_anonymous_clear_display_name_is_noop: an unlinked player
// clearing a name they cannot have must get a quiet 204, not a 403.
func TestProfile_anonymous_clear_display_name_is_noop(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "dn")
	resp, body := patchDisplayName(t, srv.URL, "dn", tok, "")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
}

// TestProfile_email_conflict_is_labeled_email: a duplicate email must not be
// reported as a xuid conflict.
func TestProfile_email_conflict_is_labeled_email(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tokA, _ := anonymousLoginWithID(t, srv.URL, "dn")
	resp, body := authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "dn", tokA,
		map[string]string{"email": "claimed@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))

	tokB, _ := anonymousLoginWithID(t, srv.URL, "dn")
	resp, body = authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "dn", tokB,
		map[string]string{"email": "claimed@example.com"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "email already in use")
	assert.NotContains(t, string(body), "xuid")
}

// ── patch atomicity ─────────────────────────────────────────────────────────

// TestProfile_patch_is_atomic_when_one_field_invalid: a PATCH that fails
// validation on any field must change nothing, even when another field in the
// same request was valid.
func TestProfile_patch_is_atomic_when_one_field_invalid(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "dn")
	srv := newServerForCluster(t, c)

	tok, _ := linkedPlayerWithName(t, c, srv.URL, "dn", "Before")

	resp, body := authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "dn", tok,
		map[string]string{"display_name": "After", "xuid": "bad\aname"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))

	assert.Equal(t, "Before", getProfile(t, srv.URL, "dn", tok).DisplayName,
		"a rejected xuid must not leave the display name half-committed")
}

// ── enrichment: session peers and leaderboard entries ───────────────────────

type namedPeerResp struct {
	SessionID string `json:"session_id"`
	Peers     []struct {
		PlayerID    int64  `json:"player_id"`
		DisplayName string `json:"display_name"`
	} `json:"peers"`
}

func TestGameSession_peers_carry_display_name_when_set(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 2, "gs")
	srv := newServerForCluster(t, c)

	hostTok, hostID := linkedPlayerWithName(t, c, srv.URL, "gs", "Hosty")
	guestTok, guestID := anonymousLoginWithID(t, srv.URL, "gs")

	created := createSession(t, srv.URL, "gs", hostTok, 4)

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, created.SessionID),
		"gs", guestTok, map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var joined namedPeerResp
	require.NoError(t, json.Unmarshal(body, &joined))
	require.Len(t, joined.Peers, 2)

	byID := map[int64]string{}
	for _, p := range joined.Peers {
		byID[p.PlayerID] = p.DisplayName
	}
	assert.Equal(t, "Hosty", byID[hostID])
	assert.Empty(t, byID[guestID])
}

func TestLeaderboard_entries_carry_display_name_when_set(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "lb2")
	ctx := context.Background()

	var board int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'named-board') RETURNING id`,
		tenantID, projectID).Scan(&board))

	srv := newServerForCluster(t, c)
	namedTok, namedID := linkedPlayerWithName(t, c, srv.URL, "lb2", "Champ")
	plainTok, plainID := anonymousLoginWithID(t, srv.URL, "lb2")

	scoresURL := fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, board)
	resp, body := authedReq(t, http.MethodPost, scoresURL, "lb2", namedTok, map[string]int64{"score": 100})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost, scoresURL, "lb2", plainTok, map[string]int64{"score": 50})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	var top struct {
		Entries []struct {
			PlayerID    int64  `json:"player_id"`
			Score       int64  `json:"score"`
			DisplayName string `json:"display_name"`
		} `json:"entries"`
	}

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top", srv.URL, board), "lb2", namedTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &top))
	require.Len(t, top.Entries, 2)

	byID := map[int64]string{}
	for _, e := range top.Entries {
		byID[e.PlayerID] = e.DisplayName
	}
	assert.Equal(t, "Champ", byID[namedID])
	assert.Empty(t, byID[plainID])

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/around-me", srv.URL, board), "lb2", plainTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &top))

	byID = map[int64]string{}
	for _, e := range top.Entries {
		byID[e.PlayerID] = e.DisplayName
	}
	assert.Equal(t, "Champ", byID[namedID])
}
