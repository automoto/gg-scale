//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ─────────────────────────────────────────────────────────────────

type friendCodeProfile struct {
	ID         int64  `json:"id"`
	FriendCode string `json:"friend_code"`
}

func getFriendCode(t *testing.T, baseURL, apiKey, token string) string {
	t.Helper()
	resp, body := authedReq(t, http.MethodGet, baseURL+"/v1/profile", apiKey, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out friendCodeProfile
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.FriendCode, "every player must have a friend code with no setup step")
	return out.FriendCode
}

func resolveFriendCode(t *testing.T, baseURL, apiKey, token, code string) (*http.Response, []byte) {
	t.Helper()
	return authedReq(t, http.MethodGet, baseURL+"/v1/players/by-code/"+code, apiKey, token, nil)
}

// ── code issuance ───────────────────────────────────────────────────────────

func TestFriendCode_minted_on_first_profile_read_and_stable(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "fc")
	first := getFriendCode(t, srv.URL, "fc", tok)
	assert.Len(t, first, 8)
	assert.Equal(t, first, getFriendCode(t, srv.URL, "fc", tok),
		"the code must be stable across reads")

	tokB, _ := anonymousLoginWithID(t, srv.URL, "fc")
	assert.NotEqual(t, first, getFriendCode(t, srv.URL, "fc", tokB))
}

func TestFriendCode_regenerate_invalidates_old_code(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "fc")
	tokB, _ := anonymousLoginWithID(t, srv.URL, "fc")
	oldCode := getFriendCode(t, srv.URL, "fc", tokA)

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/profile/friend-code", "fc", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		FriendCode string `json:"friend_code"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.NotEmpty(t, out.FriendCode)
	require.NotEqual(t, oldCode, out.FriendCode)

	resp, _ = resolveFriendCode(t, srv.URL, "fc", tokB, oldCode)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the old code must stop resolving")

	resp, body = resolveFriendCode(t, srv.URL, "fc", tokB, out.FriendCode)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var pl publicPlayerBody
	require.NoError(t, json.Unmarshal(body, &pl))
	assert.Equal(t, idA, pl.ID)
}

// ── resolve + friend request loop ───────────────────────────────────────────

func TestFriendCode_resolve_then_friend_request_full_loop(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)

	tokA, idA := linkedPlayerWithName(t, c, srv.URL, "fc", "Code Sharer")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "fc")
	linkPlayerAccount(t, c, idB)

	// A shares the code out of band; B resolves it to a public player.
	code := getFriendCode(t, srv.URL, "fc", tokA)
	resp, body := resolveFriendCode(t, srv.URL, "fc", tokB, code)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var pl publicPlayerBody
	require.NoError(t, json.Unmarshal(body, &pl))
	assert.Equal(t, idA, pl.ID)
	assert.Equal(t, "Code Sharer", pl.DisplayName)
	assert.NotContains(t, string(body), "email")

	// B sends the request via the existing route; A accepts.
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, pl.ID), "fc", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/accept", srv.URL, idB), "fc", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}

func TestFriendCode_resolve_normalizes_case_and_dashes(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "fc")
	tokB, _ := anonymousLoginWithID(t, srv.URL, "fc")
	code := getFriendCode(t, srv.URL, "fc", tokA)

	// Codes read aloud or typed by hand arrive lowercased and dashed.
	messy := strings.ToLower(code[:4]) + "-" + strings.ToLower(code[4:])
	resp, body := resolveFriendCode(t, srv.URL, "fc", tokB, messy)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var pl publicPlayerBody
	require.NoError(t, json.Unmarshal(body, &pl))
	assert.Equal(t, idA, pl.ID)
}

func TestFriendCode_unknown_code_is_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "fc")

	resp, body := resolveFriendCode(t, srv.URL, "fc", tok, "AAAA2222")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestFriendCode_cross_project_code_is_404(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)
	ctx := context.Background()

	var projectB, foreignID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'project-b') RETURNING id`,
		tenantID).Scan(&projectB))
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id, friend_code)
		 VALUES ($1, $2, 'foreign', 'ZZZZ7777') RETURNING id`,
		tenantID, projectB).Scan(&foreignID))

	tok, _ := anonymousLoginWithID(t, srv.URL, "fc")
	resp, body := resolveFriendCode(t, srv.URL, "fc", tok, "ZZZZ7777")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}
