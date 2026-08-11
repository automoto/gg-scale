//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── self-service delete request + credential-based cancel ───────────────────
//
// POST /v1/auth/delete disables the caller in this project, stamps
// delete_requested_at, and revokes every session. The request also killed the
// caller's sessions and login rejects disabled players, so the cancel endpoint
// re-authenticates with email + password instead of a session. Both no-row and
// bad-password cancel answer 404 so the endpoint doesn't confirm which emails
// have a pending deletion.

type deleteRequestResult struct {
	DeleteRequestedAt time.Time `json:"delete_requested_at"`
	ScheduledPurgeAt  time.Time `json:"scheduled_purge_at"`
}

func TestDeleteRequest_credentialed_flow(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	sess := signupVerifiedPlayer(t, srv.URL, "pw", rec, "erase@example.com", "supersecret")

	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/delete", "pw", sess.AccessToken,
		map[string]string{})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/delete", "pw", sess.AccessToken,
		map[string]string{"password": "wrongwrong"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/auth/delete", "pw", sess.AccessToken,
		map[string]string{"password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var res deleteRequestResult
	require.NoError(t, json.Unmarshal(body, &res))
	assert.False(t, res.DeleteRequestedAt.IsZero())
	assert.Equal(t, 720*time.Hour, res.ScheduledPurgeAt.Sub(res.DeleteRequestedAt),
		"purge is scheduled one default grace period after the request")

	var disabledMatchesRequest bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at = delete_requested_at FROM project_players WHERE id = $1`, sess.PlayerID).
		Scan(&disabledMatchesRequest))
	assert.True(t, disabledMatchesRequest,
		"the request must own the disable so cancel can lift it")

	// Every live credential is dead and sign-in is blocked.
	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "pw", sess.AccessToken, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"the request must kill live access tokens at the epoch gate")
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "pw",
		map[string]string{"refresh_token": sess.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, http.StatusUnauthorized, loginStatus(t, srv.URL, "pw", "erase@example.com", "supersecret"))

	var audits int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'auth.delete_request' AND actor_user_id = $1`,
		sess.PlayerID).Scan(&audits))
	assert.Equal(t, int64(1), audits)
}

func TestDeleteRequest_anonymous_immediate(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, _ := newFullStackServer(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "pw")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/delete", "pw", tok,
		map[string]string{})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/profile", "pw", tok, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	var pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT delete_requested_at IS NOT NULL FROM project_players WHERE id = $1`, id).Scan(&pending))
	assert.True(t, pending)
}

func TestDeleteCancel_credential_based(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, rec := newFullStackServer(t, c)

	sess := signupVerifiedPlayer(t, srv.URL, "pw", rec, "undo@example.com", "supersecret")
	resp, body := authedReq(t, http.MethodPost, srv.URL+"/v1/auth/delete", "pw", sess.AccessToken,
		map[string]string{"password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Opaque misses: bad password and unknown email both read as "no pending
	// deletion", so the endpoint can't be used to probe deletion state.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/delete/cancel", "pw",
		map[string]string{"email": "undo@example.com", "password": "wrongwrong"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/delete/cancel", "pw",
		map[string]string{"email": "nobody@example.com", "password": "supersecret"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/delete/cancel", "pw",
		map[string]string{"email": "undo@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	var disabled, pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at IS NOT NULL, delete_requested_at IS NOT NULL
		 FROM project_players WHERE id = $1`, sess.PlayerID).Scan(&disabled, &pending))
	assert.False(t, disabled, "cancel lifts the disable the request created")
	assert.False(t, pending)
	assert.Equal(t, http.StatusOK, loginStatus(t, srv.URL, "pw", "undo@example.com", "supersecret"))

	var audits int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'auth.delete_cancel' AND actor_user_id = $1`,
		sess.PlayerID).Scan(&audits))
	assert.Equal(t, int64(1), audits)

	// Nothing left to cancel.
	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/delete/cancel", "pw",
		map[string]string{"email": "undo@example.com", "password": "supersecret"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDeleteCancel_passwordless_pending_row_stays_opaque(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "pw")
	srv, _ := newFullStackServer(t, c)

	// An account-linked profile can carry an email without a project-local
	// password. Credential cancel must answer the same 404 as an unknown
	// email — never confirm the pending deletion, never cancel it.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id, email, disabled_at, delete_requested_at)
		 VALUES ($1, $2, 'nopass', 'nopass@example.com', now(), now())`, tenantID, projectID)
	require.NoError(t, err)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/delete/cancel", "pw",
		map[string]string{"email": "nopass@example.com", "password": "whateverpass"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))

	var pending bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT delete_requested_at IS NOT NULL FROM project_players WHERE email = 'nopass@example.com'`).
		Scan(&pending))
	assert.True(t, pending, "the pending deletion must stay in place")
}
