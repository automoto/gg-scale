//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disablePlayer flips disabled_at to now() for the given end_user.
func disablePlayer(t *testing.T, c *cluster, endUserID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE end_users SET disabled_at = now() WHERE id = $1`, endUserID)
	require.NoError(t, err)
}

// TestLogin_rejects_disabled_player covers gpt-review finding #2: disabled
// accounts must not authenticate via /v1/auth/login. Pre-fix the handler
// issued a session+JWT regardless of disabled_at.
func TestLogin_rejects_disabled_player(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "ghosted@example.com", "password": "supersecret"})
	require.Len(t, rec.Sent, 1)
	code := extractVerifyToken(t, rec.Sent[0].Body)
	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "ghosted@example.com", "code": code})

	// Sanity: login works before disable.
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "ghosted@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var endUserID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM end_users WHERE email = 'ghosted@example.com'`).Scan(&endUserID))
	disablePlayer(t, c, endUserID)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "ghosted@example.com", "password": "supersecret"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestRefresh_rejects_disabled_player covers the matching refresh path:
// a live refresh token issued before disable must not rotate into a new
// session once the underlying account is disabled.
func TestRefresh_rejects_disabled_player(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "rev@example.com", "password": "supersecret"})
	code := extractVerifyToken(t, rec.Sent[0].Body)
	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "rev@example.com", "code": code})

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "rev@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var s struct {
		RefreshToken string `json:"refresh_token"`
		EndUserID    int64  `json:"end_user_id"`
	}
	require.NoError(t, json.Unmarshal(body, &s))
	require.NotEmpty(t, s.RefreshToken)

	disablePlayer(t, c, s.EndUserID)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": s.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestVerify_replay_for_verified_user_is_idempotent_no_session: replaying
// a successful verify call must not behave like "freshly verified" and
// must never mint a session in the JSON payload. (Sessions come only
// from /login, /refresh, /custom-token.) This is the JSON-API counterpart
// of the C3 dashboard/player-UI fix that redirects already-verified
// users to login instead of issuing a session.
func TestVerify_replay_for_verified_user_is_idempotent_no_session(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "again@example.com", "password": "supersecret"})
	code := extractVerifyToken(t, rec.Sent[0].Body)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "again@example.com", "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "again@example.com", "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.NotContains(t, string(body), "refresh_token")
	assert.NotContains(t, string(body), "access_token")
}

// TestSignup_then_verify_with_wrong_code_then_correct_code regresses on
// the modulo-bias fix in verifycode.GenerateCode — the production code
// should still happily produce codes that the verify endpoint accepts.
func TestSignup_then_verify_with_wrong_code_then_correct_code(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "uni@example.com", "password": "supersecret"})
	correct := extractVerifyToken(t, rec.Sent[0].Body)

	// One wrong attempt to exercise the increment path.
	wrong := "000000"
	if wrong == correct {
		wrong = "999999"
	}
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "uni@example.com", "code": wrong})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "uni@example.com", "code": correct})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
