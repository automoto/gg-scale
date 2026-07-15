//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/verifycode"
)

// disablePlayer flips disabled_at to now() for the given player.
func disablePlayer(t *testing.T, c *cluster, playerID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET disabled_at = now() WHERE id = $1`, playerID)
	require.NoError(t, err)
}

// TestLogin_rejects_disabled_player covers gpt-review finding #2: disabled
// accounts must not authenticate via /v1/auth/login. Pre-fix the handler
// issued a session+JWT regardless of disabled_at.
func TestLogin_rejects_disabled_player(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
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

	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id FROM project_players WHERE email = 'ghosted@example.com'`).Scan(&playerID))
	disablePlayer(t, c, playerID)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "ghosted@example.com", "password": "supersecret"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestRefresh_rejects_disabled_player covers the matching refresh path:
// a live refresh token issued before disable must not rotate into a new
// session once the underlying account is disabled.
func TestRefresh_rejects_disabled_player(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
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
		PlayerID     int64  `json:"player_id"`
	}
	require.NoError(t, json.Unmarshal(body, &s))
	require.NotEmpty(t, s.RefreshToken)

	disablePlayer(t, c, s.PlayerID)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": s.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestVerify_replay_for_verified_user_returns_uniform_400 covers finding #1:
// once an account is verified its code hash is cleared, so any replay — the
// original code included — must collapse into the same 400 an unknown email
// produces. A distinguishable success would confirm "this email is a verified
// account here" (and leak its player_id) to any publishable-key holder,
// undoing the anti-enumeration cost signup (uniform 202) and login (dummy
// bcrypt) already pay. Sessions never come from verify regardless.
func TestVerify_replay_for_verified_user_returns_uniform_400(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "again@example.com", "password": "supersecret"})
	require.Len(t, rec.Sent, 1)
	code := extractVerifyToken(t, rec.Sent[0].Body)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "again@example.com", "code": code})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "again@example.com", "code": code})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "invalid email or code")
	assert.NotContains(t, string(body), "player_id")
	assert.NotContains(t, string(body), "refresh_token")
	assert.NotContains(t, string(body), "access_token")
}

// TestSignup_then_verify_with_wrong_code_then_correct_code regresses on
// the modulo-bias fix in verifycode.GenerateCode — the production code
// should still happily produce codes that the verify endpoint accepts.
func TestSignup_then_verify_with_wrong_code_then_correct_code(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
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

// TestVerify_wrong_codes_burn_attempts_and_exhaust pins the per-code budget:
// every wrong submission must persist its attempt bump even though the
// request fails (a rolled-back counter would hand an attacker unlimited
// guesses at the 10^6 code space), and the attempt after MaxAttempts must be
// throttled with 429 rather than granting more tries.
func TestVerify_wrong_codes_burn_attempts_and_exhaust(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "burn@example.com", "password": "supersecret"})
	require.Len(t, rec.Sent, 1)
	correct := extractVerifyToken(t, rec.Sent[0].Body)
	wrong := "000000"
	if wrong == correct {
		wrong = "999999"
	}

	for i := 0; i < verifycode.MaxAttempts; i++ {
		resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
			map[string]string{"email": "burn@example.com", "code": wrong})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	}

	var attempts, lifetime int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email_verification_attempts, email_verification_lifetime_attempts
		 FROM project_players WHERE email = 'burn@example.com'`).Scan(&attempts, &lifetime))
	assert.Equal(t, verifycode.MaxAttempts, attempts,
		"wrong-code attempts must survive the failed request")
	assert.Equal(t, verifycode.MaxAttempts, lifetime,
		"lifetime attempts must survive the failed request")

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "burn@example.com", "code": wrong})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "too many attempts")
}

// TestVerify_correct_code_rejected_once_attempts_exhausted: hitting the cap
// closes the window even for the right code — otherwise an attacker could
// spray guesses and still cash in the moment one lands. Attempts are
// pre-staged via SQL so the per-IP limiter stays out of the picture.
func TestVerify_correct_code_rejected_once_attempts_exhausted(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "capped@example.com", "password": "supersecret"})
	require.Len(t, rec.Sent, 1)
	correct := extractVerifyToken(t, rec.Sent[0].Body)

	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET email_verification_attempts = $1
		 WHERE email = 'capped@example.com'`, verifycode.MaxAttempts)
	require.NoError(t, err)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "capped@example.com", "code": correct})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
}

// TestSignup_duplicate_notice_email_is_throttled covers finding #2: the
// "someone tried to sign up" notice sent on a duplicate signup must be
// rate-limited per recipient, so a hostile caller can't weaponise repeated
// duplicate POSTs into an email flood against a known address.
func TestSignup_duplicate_notice_email_is_throttled(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	const email = "flood@example.com"
	// Initial signup creates the account and sends the verify-code email.
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": email, "password": "supersecret"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Two duplicate signups inside the cooldown: the first notice sends, the
	// second must be suppressed.
	for i := 0; i < 2; i++ {
		resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
			map[string]string{"email": email, "password": "supersecret"})
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
	}

	notices := 0
	for _, m := range rec.Sent {
		if strings.Contains(m.Body, "Someone tried to sign up") {
			notices++
		}
	}
	assert.Equal(t, 1, notices, "duplicate-signup notice must be throttled to one per cooldown")
}

// TestRefresh_reuse_of_rotated_token_revokes_all_sessions covers finding #3:
// replaying a refresh token that was already rotated is a theft signal, so it
// must revoke the player's whole session set and bump session_epoch (killing
// outstanding access tokens), not merely 401 the stale token.
func TestRefresh_reuse_of_rotated_token_revokes_all_sessions(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "reuse@example.com", "password": "supersecret"})
	require.GreaterOrEqual(t, len(rec.Sent), 1)
	code := extractVerifyToken(t, rec.Sent[0].Body)
	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "reuse@example.com", "code": code})

	_, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "reuse@example.com", "password": "supersecret"})
	var s1 struct {
		RefreshToken string `json:"refresh_token"`
		PlayerID     int64  `json:"player_id"`
	}
	require.NoError(t, json.Unmarshal(body, &s1))

	var epochBefore int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT session_epoch FROM project_players WHERE id = $1`, s1.PlayerID).Scan(&epochBefore))

	// Rotate R1 -> R2.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": s1.RefreshToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var s2 struct {
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(body, &s2))

	// Replay the old, already-rotated R1: reuse detected -> 401.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": s1.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// The freshly-issued R2 must now be dead too — the whole family is revoked.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": s2.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "reuse must revoke the whole family")

	var epochAfter int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT session_epoch FROM project_players WHERE id = $1`, s1.PlayerID).Scan(&epochAfter))
	assert.Greater(t, epochAfter, epochBefore, "reuse detection must bump session_epoch")
}

// TestRefresh_replay_after_logout_leaves_other_sessions covers the no-false-
// positive half of finding #3: a token revoked by logout is a benign stale
// retry, so replaying it must 401 WITHOUT nuking the player's other sessions.
func TestRefresh_replay_after_logout_leaves_other_sessions(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "twodev@example.com", "password": "supersecret"})
	require.GreaterOrEqual(t, len(rec.Sent), 1)
	code := extractVerifyToken(t, rec.Sent[0].Body)
	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"email": "twodev@example.com", "code": code})

	login := func() string {
		_, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
			map[string]string{"email": "twodev@example.com", "password": "supersecret"})
		var s struct {
			RefreshToken string `json:"refresh_token"`
		}
		require.NoError(t, json.Unmarshal(body, &s))
		return s.RefreshToken
	}
	rA := login()
	rB := login()

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/logout", "k",
		map[string]string{"refresh_token": rA})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Replaying the logged-out token: a benign stale retry -> 401, no family kill.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": rA})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Device B is untouched and still refreshes.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": rB})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}
