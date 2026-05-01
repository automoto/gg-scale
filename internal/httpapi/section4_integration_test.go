//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func newFullStackServer(t *testing.T, c *cluster) (*httptest.Server, *mailer.Recorder) {
	t.Helper()
	// Surface server-side errors in test output.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	rec := &mailer.Recorder{}

	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test",
		Pool:    db.NewPool(c.appPool),
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer:  signer,
		Mailer:  rec,
		Cache:   c.cache,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, rec
}

func doJSON(t *testing.T, method, url, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func authedReq(t *testing.T, method, url, bearer, sessionToken string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Session-Token", sessionToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, raw
}

func anonymousLogin(t *testing.T, baseURL, apiKey string) string {
	tok, _ := anonymousLoginWithID(t, baseURL, apiKey)
	return tok
}

func anonymousLoginWithID(t *testing.T, baseURL, apiKey string) (string, int64) {
	t.Helper()
	resp, body := doJSON(t, http.MethodPost, baseURL+"/v1/auth/anonymous", apiKey, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		AccessToken string `json:"access_token"`
		EndUserID   int64  `json:"end_user_id"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.AccessToken, out.EndUserID
}

func extractVerifyToken(t *testing.T, body string) string {
	t.Helper()
	const marker = "valid 24h):\n\n"
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "verify body shape changed: %q", body)
	rest := body[i+len(marker):]
	end := strings.Index(rest, "\n")
	require.Greater(t, end, 0)
	return rest[:end]
}

// -------- Auth flow --------

func TestSignup_then_verify_then_login_then_refresh_then_logout(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "alice@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Len(t, rec.Sent, 1)
	verifyToken := extractVerifyToken(t, rec.Sent[0].Body)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"token": verifyToken})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "alice@example.com", "password": "supersecret"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var session struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		EndUserID    int64  `json:"end_user_id"`
	}
	require.NoError(t, json.Unmarshal(body, &session))
	require.NotEmpty(t, session.AccessToken)

	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": session.RefreshToken})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(body, &refreshed))
	assert.NotEqual(t, session.RefreshToken, refreshed.RefreshToken)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "k",
		map[string]string{"refresh_token": session.RefreshToken})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/logout", "k",
		map[string]string{"refresh_token": refreshed.RefreshToken})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestSignup_rejects_duplicate_email(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "dup@example.com", "password": "supersecret"})
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "dup@example.com", "password": "supersecret"})

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestLogin_with_wrong_password_returns_401(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, rec := newFullStackServer(t, c)

	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "bob@example.com", "password": "rightpass1"})
	verifyToken := extractVerifyToken(t, rec.Sent[0].Body)
	_, _ = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/verify", "k",
		map[string]string{"token": verifyToken})

	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "bob@example.com", "password": "wrongpass"})

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestLogin_with_nonexistent_email_returns_401_with_bcrypt_timing(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	start := time.Now()
	resp, _ := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "ghost@example.com", "password": "anything"})
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Greater(t, elapsed, 100*time.Millisecond,
		"login on missing email must run a dummy bcrypt to prevent enumeration")
}

func TestCustomToken_mints_session_for_external_user(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	secret := []byte("tenant-custom-token-shared-secret")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE tenants SET custom_token_secret = $1 WHERE id = $2`, secret, tenantID)
	require.NoError(t, err)

	srv, _ := newFullStackServer(t, c)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"external_id": "steam_99",
		"exp":         time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString(secret)
	require.NoError(t, err)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/custom-token", "k",
		map[string]string{"token": signed})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var session struct {
		EndUserID int64 `json:"end_user_id"`
	}
	require.NoError(t, json.Unmarshal(body, &session))
	assert.Greater(t, session.EndUserID, int64(0))

	var got string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT external_id FROM end_users WHERE id = $1 AND project_id = $2`,
		session.EndUserID, projectID).Scan(&got))
	assert.Equal(t, "steam_99", got)
}

// -------- Storage --------

func TestStorage_put_get_delete_round_trip(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)
	access := anonymousLogin(t, srv.URL, "k")

	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/storage/objects/save", "k", access,
		map[string]any{"hp": 100})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var put struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, json.Unmarshal(body, &put))
	assert.Equal(t, int64(1), put.Version)

	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/storage/objects/save", "k", access, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Value   json.RawMessage `json:"value"`
		Version int64           `json:"version"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	assert.JSONEq(t, `{"hp":100}`, string(got.Value))
	assert.Equal(t, int64(1), got.Version)

	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/storage/objects/save", "k", access,
		map[string]any{"hp": 50})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body, &put))
	assert.Equal(t, int64(2), put.Version)

	resp, _ = authedReq(t, http.MethodDelete, srv.URL+"/v1/storage/objects/save", "k", access, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/storage/objects/save", "k", access, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStorage_if_match_blocks_stale_writes(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)
	access := anonymousLogin(t, srv.URL, "k")

	resp, _ := authedReq(t, http.MethodPut, srv.URL+"/v1/storage/objects/x", "k", access,
		map[string]any{"v": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/storage/objects/x",
		bytes.NewReader([]byte(`{"v":2}`)))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("X-Session-Token", access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "0")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
}

// -------- Leaderboards --------

func TestLeaderboard_submit_then_top_returns_best_score_per_user(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	var leaderboardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'main') RETURNING id`,
		tenantID, projectID).Scan(&leaderboardID))

	a := anonymousLogin(t, srv.URL, "k")
	b := anonymousLogin(t, srv.URL, "k")

	for _, sc := range []struct {
		token string
		score int64
	}{{a, 100}, {a, 50}, {b, 75}} {
		resp, _ := authedReq(t, http.MethodPost,
			fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, leaderboardID),
			"k", sc.token, map[string]int64{"score": sc.score})
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	}

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv.URL, leaderboardID),
		"k", a, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var top struct {
		Entries []struct {
			Score int64 `json:"score"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &top))
	require.Len(t, top.Entries, 2)
	assert.Equal(t, int64(100), top.Entries[0].Score)
	assert.Equal(t, int64(75), top.Entries[1].Score)
}

// -------- Friends --------

func TestFriends_request_accept_list(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")

	resp, _ := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, _ = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/accept", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := authedReq(t, http.MethodGet,
		srv.URL+"/v1/friends/?status=accepted", "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var list struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, "accepted", list.Items[0].Status)
}

func TestFriends_re_request_after_rejection_transitions_to_pending(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")

	_, _ = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	_, _ = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/reject", srv.URL, idA), "k", tokB, nil)

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "pending", got.Status)
}

// -------- Profile --------

func TestProfile_get_returns_calling_user(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv, _ := newFullStackServer(t, c)

	tok, id := anonymousLoginWithID(t, srv.URL, "k")

	resp, body := authedReq(t, http.MethodGet, srv.URL+"/v1/profile/", "k", tok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got struct {
		ID         int64  `json:"id"`
		ExternalID string `json:"external_id"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, id, got.ID)
}
