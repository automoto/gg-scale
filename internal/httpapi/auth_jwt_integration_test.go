//go:build integration

package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
)

// After email signup + verify + login, the access token must parse and verify
// as an HS256 JWT with the app's signing key (Recorder mailer stands in for SMTP).

func TestLogin_access_token_verifies_with_hmac_signer(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "jwtverify@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, rec.Sent, 1)
	verifyToken := extractVerifyToken(t, rec.Sent[0].Body)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"token": verifyToken})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "jwtverify@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var session struct {
		AccessToken string `json:"access_token"`
		EndUserID   int64  `json:"end_user_id"`
	}
	require.NoError(t, json.Unmarshal(body, &session))
	require.NotEmpty(t, session.AccessToken)

	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	claims, err := signer.Verify(session.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, session.EndUserID, claims.EndUserID)
	assert.Greater(t, claims.TenantID, int64(0))
	assert.False(t, claims.ExpiresAt.IsZero())
}
