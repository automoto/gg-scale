//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
)

// signSession issues a session JWT with the claims a real /v1/auth
// flow would have set. ExpiresAt is now+exp.
func signSession(t *testing.T, signer *auth.Signer, tenantID, projectID, endUserID int64, exp time.Duration) string {
	t.Helper()
	tok, err := signer.Sign(auth.Claims{
		TenantID:  tenantID,
		ProjectID: projectID,
		EndUserID: endUserID,
		ExpiresAt: time.Now().Add(exp),
	})
	require.NoError(t, err)
	return tok
}

func insertEndUser(t *testing.T, c *cluster, tenantID, projectID int64, externalID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO end_users (tenant_id, project_id, external_id) VALUES ($1, $2, $3) RETURNING id`,
		tenantID, projectID, externalID).Scan(&id))
	return id
}

func disableEndUser(t *testing.T, c *cluster, endUserID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE end_users SET disabled_at = now() WHERE id = $1`, endUserID)
	require.NoError(t, err)
}

func deleteEndUser(t *testing.T, c *cluster, endUserID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE end_users SET deleted_at = now() WHERE id = $1`, endUserID)
	require.NoError(t, err)
}

func softDeleteProject(t *testing.T, c *cluster, projectID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE projects SET deleted_at = now() WHERE id = $1`, projectID)
	require.NoError(t, err)
}

func postVerify(t *testing.T, srvURL, apiKey, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/end-users/verify", strings.NewReader(body))
	require.NoError(t, err)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func verifyBody(t *testing.T, sessionToken string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"session_token": sessionToken})
	require.NoError(t, err)
	return string(b)
}

// assertOpaqueInvalidSession asserts the wire shape every failure
// mode of /v1/end-users/verify must produce: 401, application/json,
// body == {"error":"invalid session"}, no PII leakage.
func assertOpaqueInvalidSession(t *testing.T, resp *http.Response) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, map[string]any{"error": "invalid session"}, got,
		"401 body must be opaque — no PII / state leakage")
}

func TestEndUsersVerify_returns_user_info_for_valid_session_token(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-valid")
	endUserID := insertEndUser(t, c, tenantID, projectID, "player-42")
	srv := newServerForCluster(t, c)

	tok := signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)

	resp := postVerify(t, srv.URL, "verify-valid", verifyBody(t, tok))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, float64(endUserID), got["user_id"])
	assert.Equal(t, "player-42", got["external_id"])
}

// Table-driven negative cases that share the same setup shape: seed a
// tenant + project + api key, seed an end_user, sign a token, mutate
// (or skip), POST → assert opaque 401.
func TestEndUsersVerify_rejects_invalid_sessions(t *testing.T) {
	cases := []struct {
		name string
		// mutate runs after seeding but before the request. Use it to
		// expire/tamper the token, soft-delete the user, etc.
		mutate func(t *testing.T, c *cluster, tenantID, projectID, endUserID int64) string
	}{
		{
			name: "expired_token",
			mutate: func(t *testing.T, _ *cluster, tenantID, projectID, endUserID int64) string {
				return signSession(t, newTestSigner(t), tenantID, projectID, endUserID, -time.Hour)
			},
		},
		{
			name: "tampered_signature",
			mutate: func(t *testing.T, _ *cluster, tenantID, projectID, endUserID int64) string {
				other, err := auth.NewSigner([]byte("other-key-must-be-at-least-32-bytes!"))
				require.NoError(t, err)
				return signSession(t, other, tenantID, projectID, endUserID, time.Hour)
			},
		},
		{
			name: "deleted_user",
			mutate: func(t *testing.T, c *cluster, tenantID, projectID, endUserID int64) string {
				deleteEndUser(t, c, endUserID)
				return signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)
			},
		},
		{
			name: "disabled_user",
			mutate: func(t *testing.T, c *cluster, tenantID, projectID, endUserID int64) string {
				disableEndUser(t, c, endUserID)
				return signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)
			},
		},
		{
			name: "soft_deleted_project",
			mutate: func(t *testing.T, c *cluster, tenantID, projectID, endUserID int64) string {
				softDeleteProject(t, c, projectID)
				return signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)
			},
		},
		{
			name: "project_id_zero_bypass",
			// Token forged with ProjectID=0 must NOT skip the project
			// pinning check.
			mutate: func(t *testing.T, _ *cluster, tenantID, _, endUserID int64) string {
				return signSession(t, newTestSigner(t), tenantID, 0, endUserID, time.Hour)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-"+tc.name)
			endUserID := insertEndUser(t, c, tenantID, projectID, "player-"+tc.name)
			srv := newServerForCluster(t, c)

			tok := tc.mutate(t, c, tenantID, projectID, endUserID)

			resp := postVerify(t, srv.URL, "verify-"+tc.name, verifyBody(t, tok))
			defer resp.Body.Close()
			assertOpaqueInvalidSession(t, resp)
		})
	}
}

// Body-shape negative cases share a separate harness because no token
// is signed.
func TestEndUsersVerify_rejects_bad_request_bodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_body", ""},
		{"empty_token", `{"session_token":""}`},
		{"missing_field", `{}`},
		{"unknown_field", `{"session_token":"abc","extra":"nope"}`},
		{"malformed_json", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-"+tc.name)
			srv := newServerForCluster(t, c)

			req, err := http.NewRequest(http.MethodPost,
				srv.URL+"/v1/end-users/verify",
				bytes.NewReader([]byte(tc.body)))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer verify-"+tc.name)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assertOpaqueInvalidSession(t, resp)
		})
	}
}

func TestEndUsersVerify_rejects_oversized_body(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-toobig")
	srv := newServerForCluster(t, c)

	// 16 KiB > the 8 KiB handler cap.
	huge := strings.Repeat("a", 16<<10)
	resp := postVerify(t, srv.URL, "verify-toobig", verifyBody(t, huge))
	defer resp.Body.Close()
	assertOpaqueInvalidSession(t, resp)
}

func TestEndUsersVerify_rejects_cross_tenant_token(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-xtenant-a")
	tenantB, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-xtenant-b")
	endUserB := insertEndUser(t, c, tenantB, projectB, "player-b")
	srv := newServerForCluster(t, c)

	// Token issued under tenant B presented by tenant A's API key.
	tok := signSession(t, newTestSigner(t), tenantB, projectB, endUserB, time.Hour)
	resp := postVerify(t, srv.URL, "verify-xtenant-a", verifyBody(t, tok))
	defer resp.Body.Close()
	assertOpaqueInvalidSession(t, resp)
}

// Same tenant, different project — caller's API key is pinned to
// project A, token is for project B's end_user. Must be rejected.
func TestEndUsersVerify_rejects_cross_project_token_within_tenant(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-xproj-a")
	// Second project under the same tenant.
	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
		tenantID, "project-verify-xproj-b").Scan(&projectB))
	endUserB := insertEndUser(t, c, tenantID, projectB, "player-b")
	srv := newServerForCluster(t, c)

	// Token issued for project B but presented by project A's API key.
	tok := signSession(t, newTestSigner(t), tenantID, projectB, endUserB, time.Hour)
	resp := postVerify(t, srv.URL, "verify-xproj-a", verifyBody(t, tok))
	defer resp.Body.Close()
	assertOpaqueInvalidSession(t, resp)
}

func TestEndUsersVerify_requires_api_key_auth(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-nokey")
	endUserID := insertEndUser(t, c, tenantID, projectID, "player-anon")
	srv := newServerForCluster(t, c)

	tok := signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)
	resp := postVerify(t, srv.URL, "", verifyBody(t, tok))
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// Publishable keys are embedded in shipped game binaries; the verify
// endpoint must refuse them so a leaked publishable key can't be used
// as a session-validity oracle.
func TestEndUsersVerify_rejects_publishable_key(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "verify-secret")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "verify-pub", "publishable")
	endUserID := insertEndUser(t, c, tenantID, projectID, "player-pub")
	srv := newServerForCluster(t, c)

	tok := signSession(t, newTestSigner(t), tenantID, projectID, endUserID, time.Hour)
	resp := postVerify(t, srv.URL, "verify-pub", verifyBody(t, tok))
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}
