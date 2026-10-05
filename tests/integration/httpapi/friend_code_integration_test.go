//go:build integration

// e2e:bucket a

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

func TestFriends_list_should_show_email_only_for_accepted_friends(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)
	tokA, idA := anonymousLoginWithID(t, srv.URL, "fc")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "fc")
	linkPlayerAccount(t, c, idA)
	linkPlayerAccount(t, c, idB)
	emailB := fmt.Sprintf("linked-%d@example.com", idB)
	resp, body := authedReq(t, http.MethodPost, fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "fc", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	t.Run("should_hide_email_of_pending_target", func(t *testing.T) {
		_, body := authedReq(t, http.MethodGet, srv.URL+"/v1/friends?status=pending", "fc", tokA, nil)
		require.Contains(t, string(body), `"status":"pending"`)
		assert.NotContains(t, string(body), emailB)
	})
	t.Run("should_show_email_once_accepted", func(t *testing.T) {
		resp, body := authedReq(t, http.MethodPost, fmt.Sprintf("%s/v1/friends/%d/accept", srv.URL, idA), "fc", tokB, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		_, body = authedReq(t, http.MethodGet, srv.URL+"/v1/friends", "fc", tokA, nil)
		assert.Contains(t, string(body), emailB)
	})
}

func TestFriendCode_hidden_target_is_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)
	tokCaller, idCaller := anonymousLoginWithID(t, srv.URL, "fc")
	linkPlayerAccount(t, c, idCaller)

	cases := []struct {
		name string
		hide func(t *testing.T, tok string, id int64)
	}{
		{"disabled", func(t *testing.T, _ string, id int64) {
			_, err := c.bootstrapPool.Exec(context.Background(),
				`UPDATE project_players SET disabled_at = now() WHERE id = $1`, id)
			require.NoError(t, err)
		}},
		{"delete_requested", func(t *testing.T, _ string, id int64) {
			_, err := c.bootstrapPool.Exec(context.Background(),
				`UPDATE project_players SET delete_requested_at = now(), disabled_at = now() WHERE id = $1`, id)
			require.NoError(t, err)
		}},
		{"target_blocked_caller", func(t *testing.T, tok string, _ int64) {
			resp, body := authedReq(t, http.MethodPost,
				fmt.Sprintf("%s/v1/friends/%d/block", srv.URL, idCaller), "fc", tok, nil)
			require.Less(t, resp.StatusCode, http.StatusMultipleChoices, string(body))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, id := anonymousLoginWithID(t, srv.URL, "fc")
			linkPlayerAccount(t, c, id)
			code := getFriendCode(t, srv.URL, "fc", tok)
			tc.hide(t, tok, id)

			resp, body := resolveFriendCode(t, srv.URL, "fc", tokCaller, code)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
		})
	}
}

func TestFriendCode_malformed_code_is_404(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "fc")
	srv := newServerForCluster(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "fc")

	for _, code := range []string{"%00", "AB%00CD", "AAAA222", "%C3%A9AAA2222"} {
		t.Run(code, func(t *testing.T) {
			resp, body := resolveFriendCode(t, srv.URL, "fc", tok, code)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
		})
	}
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
