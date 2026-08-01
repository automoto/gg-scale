//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/mailer"
)

// ── helpers ─────────────────────────────────────────────────────────────────

type authSession struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	PlayerID     int64  `json:"player_id"`
}

// signupVerifiedPlayer runs signup → verify and returns a logged-in session.
func signupVerifiedPlayer(t *testing.T, srvURL, apiKey string, rec *mailer.Recorder, email, password string) authSession {
	t.Helper()
	resp, body := doJSON(t, http.MethodPost, srvURL+"/v1/auth/signup", apiKey,
		map[string]string{"email": email, "password": password})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
	code := extractVerifyToken(t, rec.Sent[len(rec.Sent)-1].Body)
	resp, body = doJSON(t, http.MethodPost, srvURL+"/v1/auth/verify", apiKey,
		map[string]string{"email": email, "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return loginPlayer(t, srvURL, apiKey, email, password)
}

func loginPlayer(t *testing.T, srvURL, apiKey, email, password string) authSession {
	t.Helper()
	resp, body := doJSON(t, http.MethodPost, srvURL+"/v1/auth/login", apiKey,
		map[string]string{"email": email, "password": password})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var s authSession
	require.NoError(t, json.Unmarshal(body, &s))
	return s
}

func loginStatus(t *testing.T, srvURL, apiKey, email, password string) int {
	t.Helper()
	resp, _ := doJSON(t, http.MethodPost, srvURL+"/v1/auth/login", apiKey,
		map[string]string{"email": email, "password": password})
	return resp.StatusCode
}

// ── password reset (request + confirm) ──────────────────────────────────────

func TestPasswordReset_full_in_client_flow(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	sess := signupVerifiedPlayer(t, srv.URL, "pw", rec, "reset@example.com", "oldpassword")

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset", "pw",
		map[string]string{"email": "reset@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
	code := extractVerifyToken(t, rec.Sent[len(rec.Sent)-1].Body)

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset/confirm", "pw",
		map[string]string{"email": "reset@example.com", "code": code, "new_password": "newpassword"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, srv.URL, "pw", "reset@example.com", "oldpassword"))
	fresh := loginPlayer(t, srv.URL, "pw", "reset@example.com", "newpassword")
	assert.Equal(t, sess.PlayerID, fresh.PlayerID)

	// Every pre-reset credential is dead: refresh token and live access token.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "pw",
		map[string]string{"refresh_token": sess.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "old refresh token must be revoked")
	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "pw", sess.AccessToken, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "old access token must die at the epoch gate")
}

func TestPasswordReset_unknown_email_is_opaque(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset", "pw",
		map[string]string{"email": "nobody@example.com"})
	assert.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))
	assert.Empty(t, rec.Sent, "an unknown email must not produce mail")
}

func TestPasswordReset_wrong_codes_hit_attempt_cap(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	signupVerifiedPlayer(t, srv.URL, "pw", rec, "cap@example.com", "oldpassword")
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset", "pw",
		map[string]string{"email": "cap@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	code := extractVerifyToken(t, rec.Sent[len(rec.Sent)-1].Body)

	for range 5 {
		resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset/confirm", "pw",
			map[string]string{"email": "cap@example.com", "code": "000000", "new_password": "newpassword"})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	// The cap blocks even the correct code now.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset/confirm", "pw",
		map[string]string{"email": "cap@example.com", "code": code, "new_password": "newpassword"})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
}

func TestPasswordReset_bad_new_password_does_not_burn_the_code(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	signupVerifiedPlayer(t, srv.URL, "pw", rec, "burn@example.com", "oldpassword")
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset", "pw",
		map[string]string{"email": "burn@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	code := extractVerifyToken(t, rec.Sent[len(rec.Sent)-1].Body)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset/confirm", "pw",
		map[string]string{"email": "burn@example.com", "code": code, "new_password": "short"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset/confirm", "pw",
		map[string]string{"email": "burn@example.com", "code": code, "new_password": "newpassword"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
}

func TestPasswordReset_request_cooldown_sends_one_mail(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	signupVerifiedPlayer(t, srv.URL, "pw", rec, "cool@example.com", "oldpassword")
	mailsBefore := len(rec.Sent)
	for range 2 {
		resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/password-reset", "pw",
			map[string]string{"email": "cool@example.com"})
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
	}
	assert.Equal(t, mailsBefore+1, len(rec.Sent),
		"a second request inside the cooldown must stay opaque and send nothing")
}

// ── resend verification ─────────────────────────────────────────────────────

func TestVerifyResend_fresh_code_replaces_the_old_one(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "pw",
		map[string]string{"email": "resend@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	oldCode := extractVerifyToken(t, rec.Sent[0].Body)

	// Cooldown holds first; backdate the send to get past it.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify/resend", "pw",
		map[string]string{"email": "resend@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, rec.Sent, 1, "resend inside the cooldown must not mail")

	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET email_verification_last_sent_at = now() - interval '2 minutes'
		 WHERE email = 'resend@example.com'`)
	require.NoError(t, err)
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify/resend", "pw",
		map[string]string{"email": "resend@example.com"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, rec.Sent, 2)
	newCode := extractVerifyToken(t, rec.Sent[1].Body)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "pw",
		map[string]string{"email": "resend@example.com", "code": oldCode})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "the old code must be replaced")
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "pw",
		map[string]string{"email": "resend@example.com", "code": newCode})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}

func TestVerifyResend_opaque_for_unknown_and_verified(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	signupVerifiedPlayer(t, srv.URL, "pw", rec, "done@example.com", "supersecret")
	mailsBefore := len(rec.Sent)

	for _, email := range []string{"nobody@example.com", "done@example.com"} {
		resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify/resend", "pw",
			map[string]string{"email": email})
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	}
	assert.Equal(t, mailsBefore, len(rec.Sent))
}

// ── change password ─────────────────────────────────────────────────────────

func TestChangePassword_full_flow(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	sess := signupVerifiedPlayer(t, srv.URL, "pw", rec, "change@example.com", "oldpassword")

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/password", "pw", sess.AccessToken,
		map[string]string{"current_password": "wrongwrong", "new_password": "newpassword"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/password", "pw", sess.AccessToken,
		map[string]string{"current_password": "oldpassword", "new_password": "newpassword"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, srv.URL, "pw", "change@example.com", "oldpassword"))
	assert.Equal(t, http.StatusOK, loginStatus(t, srv.URL, "pw", "change@example.com", "newpassword"))

	// Refresh tokens are revoked; the current access token survives its TTL.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "pw",
		map[string]string{"refresh_token": sess.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "pw", sess.AccessToken, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"change-password must not kill the caller's live access token")
}

func TestChangePassword_anonymous_player_has_no_credentials(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, _ := newFullStackServer(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "pw")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/password", "pw", tok,
		map[string]string{"current_password": "whatever1", "new_password": "newpassword"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "link")
}

// ── self-service disable ────────────────────────────────────────────────────

func TestDisable_anonymous_player_immediate(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, _ := newFullStackServer(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "pw")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/disable", "pw", tok,
		map[string]string{})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "pw", tok, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"disable must kill live access tokens at the epoch gate")

	var disabled bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at IS NOT NULL FROM project_players WHERE id = $1`, id).Scan(&disabled))
	assert.True(t, disabled)
}

func TestDisable_credentialed_requires_password_and_admin_reenable_restores(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	sess := signupVerifiedPlayer(t, srv.URL, "pw", rec, "gone@example.com", "supersecret")

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/disable", "pw", sess.AccessToken,
		map[string]string{})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/disable", "pw", sess.AccessToken,
		map[string]string{"password": "wrongwrong"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/disable", "pw", sess.AccessToken,
		map[string]string{"password": "supersecret"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))
	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, srv.URL, "pw", "gone@example.com", "supersecret"))

	// Admin re-enable (control panel path) restores the account with data intact.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET disabled_at = NULL WHERE id = $1`, sess.PlayerID)
	require.NoError(t, err)
	restored := loginPlayer(t, srv.URL, "pw", "gone@example.com", "supersecret")
	assert.Equal(t, sess.PlayerID, restored.PlayerID)
}
