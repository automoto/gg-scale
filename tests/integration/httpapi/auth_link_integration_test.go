//go:build integration

// e2e:bucket a

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

func linkEmail(t *testing.T, baseURL, apiKey, token, email, password string) (*http.Response, []byte) {
	t.Helper()
	return authedReq(t, http.MethodPost, baseURL+"/v1/auth/link", apiKey, token,
		map[string]string{"email": email, "password": password})
}

func playerAccountID(t *testing.T, c *cluster, playerID int64) *string {
	t.Helper()
	var acc *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT player_account_id::text FROM project_players WHERE id = $1`, playerID).Scan(&acc))
	return acc
}

func playerEmail(t *testing.T, c *cluster, playerID int64) *string {
	t.Helper()
	var email *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email FROM project_players WHERE id = $1`, playerID).Scan(&email))
	return email
}

// ── email linking ───────────────────────────────────────────────────────────

func TestAuthLink_email_full_upgrade_flow(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	srv, rec := newFullStackServer(t, c)

	tok, anonID := anonymousLoginWithID(t, srv.URL, "lk")

	// The anonymous player owns data before linking.
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/storage/objects/save1", "lk", tok,
		map[string]any{"value": map[string]any{"level": 9}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = linkEmail(t, srv.URL, "lk", tok, "upgrade@example.com", "supersecret")
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
	require.Len(t, rec.Sent, 1)
	code := extractVerifyToken(t, rec.Sent[0].Body)

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "lk",
		map[string]string{"email": "upgrade@example.com", "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Second device: log in with the linked credentials.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "lk",
		map[string]string{"email": "upgrade@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var session struct {
		AccessToken string `json:"access_token"`
		PlayerID    int64  `json:"player_id"`
	}
	require.NoError(t, json.Unmarshal(body, &session))
	assert.Equal(t, anonID, session.PlayerID, "linking must keep the player id")

	// The pre-link data is intact under the new session.
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/storage/objects/save1", "lk",
		session.AccessToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"level":9`)

	// Verification attached the global account: display names unlock.
	require.NotNil(t, playerAccountID(t, c, anonID), "verify must attach a player_accounts row")
	resp, body = patchDisplayName(t, srv.URL, "lk", tok, "Upgraded")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
}

func TestAuthLink_email_taken_in_project_is_409_and_changes_nothing(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	srv, _ := newFullStackServer(t, c)

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "lk",
		map[string]string{"email": "taken@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	tok, anonID := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body := linkEmail(t, srv.URL, "lk", tok, "taken@example.com", "anotherpass")
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "already in use")
	assert.Nil(t, playerEmail(t, c, anonID), "a rejected link must change nothing")
}

func TestAuthLink_player_with_credentials_is_409(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	srv, _ := newFullStackServer(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body := linkEmail(t, srv.URL, "lk", tok, "first@example.com", "supersecret")
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))

	resp, body = linkEmail(t, srv.URL, "lk", tok, "second@example.com", "supersecret")
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "already has sign-in credentials")
}

func TestAuthLink_invalid_input_is_400(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	srv, _ := newFullStackServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "lk")

	cases := []struct {
		name, email, password string
	}{
		{"bad_email", "not-an-email", "supersecret"},
		{"short_password", "ok@example.com", "short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := linkEmail(t, srv.URL, "lk", tok, tc.email, tc.password)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
		})
	}
}

func TestAuthVerify_attaches_existing_global_account(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	srv, rec := newFullStackServer(t, c)

	var existingAcc string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ('shared@example.com', '\x00'::bytea, now()) RETURNING id::text`).Scan(&existingAcc))

	tok, anonID := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body := linkEmail(t, srv.URL, "lk", tok, "shared@example.com", "supersecret")
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
	code := extractVerifyToken(t, rec.Sent[len(rec.Sent)-1].Body)

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "lk",
		map[string]string{"email": "shared@example.com", "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	acc := playerAccountID(t, c, anonID)
	require.NotNil(t, acc)
	assert.Equal(t, existingAcc, *acc,
		"a proven email must attach to the existing global account, not mint a duplicate")
}

// ── steam linking ───────────────────────────────────────────────────────────

func TestAuthLinkSteam_upgrades_anonymous_identity(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	tok, anonID := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/link/steam", "lk", tok,
		map[string]string{"ticket": steamTestTicket})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	var externalID string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT external_id FROM project_players WHERE id = $1`, anonID).Scan(&externalID))
	assert.Equal(t, "steam:"+steamTestID, externalID)

	// A later native Steam sign-in resolves to the linked player.
	resp, body = postSteamAuth(t, srv.URL, "lk", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out steamSessionBody
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, anonID, out.PlayerID)
}

func TestAuthLinkSteam_identity_taken_is_409(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	// Player B claims the Steam identity via native sign-in first.
	resp, body := postSteamAuth(t, srv.URL, "lk", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	tok, anonID := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/link/steam", "lk", tok,
		map[string]string{"ticket": steamTestTicket})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))

	var externalID string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT external_id FROM project_players WHERE id = $1`, anonID).Scan(&externalID))
	assert.Contains(t, externalID, "anon_", "a rejected link must keep the old identity")
}

func TestAuthLinkSteam_custom_token_identity_is_never_replaced(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	priv := seedCustomTokenKey(t, c, tenantID)
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/custom-token", "lk",
		map[string]string{"token": signCustomToken(t, priv, "dev-keyed-77")})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var session struct {
		AccessToken string `json:"access_token"`
		PlayerID    int64  `json:"player_id"`
	}
	require.NoError(t, json.Unmarshal(body, &session))

	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/link/steam", "lk",
		session.AccessToken, map[string]string{"ticket": steamTestTicket})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))

	var externalID string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT external_id FROM project_players WHERE id = $1`, session.PlayerID).Scan(&externalID))
	assert.Equal(t, "dev-keyed-77", externalID,
		"a developer-keyed identity must never be overwritten")
}

func TestAuthLinkSteam_unconfigured_project_is_400(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	valve, calls := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, _ := newServerWithSteamVerifier(t, c, valve.URL)

	tok, _ := anonymousLoginWithID(t, srv.URL, "lk")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/link/steam", "lk", tok,
		map[string]string{"ticket": steamTestTicket})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	assert.Zero(t, calls.Load())
}

func TestAuthLinkSteam_repeat_link_same_identity_is_idempotent(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "lk")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	tok, _ := anonymousLoginWithID(t, srv.URL, "lk")
	for i := 0; i < 2; i++ {
		resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/link/steam", "lk", tok,
			map[string]string{"ticket": steamTestTicket})
		require.Equal(t, http.StatusNoContent, resp.StatusCode,
			fmt.Sprintf("attempt %d: %s", i+1, string(body)))
	}

	var n int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE external_id = $1`,
		"steam:"+steamTestID).Scan(&n))
	assert.Equal(t, int64(1), n)
}
