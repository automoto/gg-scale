//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAPIKey inserts an api_keys row of the given key_type for an existing
// tenant + project. Counterpart to seedTenantWithAPIKey, which always
// creates a secret-typed key.
func seedAPIKey(t *testing.T, pool *pgxpool.Pool, tenantID int64, projectID *int64, token, keyType string) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type) VALUES ($1, $2, $3, $4)`,
		tenantID, projectID, sum[:], keyType)
	require.NoError(t, err)
}

func TestKeyType_publishable_blocked_from_fleet_register(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "publishable-k", "publishable")
	srv, _ := newFullStackServer(t, c)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/fleet/servers", "publishable-k",
		map[string]any{"address": "host:1", "name": "gs-1"})

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestKeyType_secret_allowed_to_register_fleet(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")
	srv, _ := newFullStackServer(t, c)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/fleet/servers", "secret-k",
		map[string]any{"address": "host:1", "name": "gs-1"})

	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
}

func TestKeyType_publishable_blocked_from_leaderboard_submit(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "publishable-k", "publishable")

	var leaderboardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'main') RETURNING id`,
		tenantID, projectID).Scan(&leaderboardID))

	srv, _ := newFullStackServer(t, c)
	access := anonymousLogin(t, srv.URL, "publishable-k")

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, leaderboardID),
		"publishable-k", access, map[string]int64{"score": 99999})

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestKeyType_secret_allowed_for_leaderboard_submit(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")

	var leaderboardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'main') RETURNING id`,
		tenantID, projectID).Scan(&leaderboardID))

	srv, _ := newFullStackServer(t, c)
	access := anonymousLogin(t, srv.URL, "secret-k")

	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, leaderboardID),
		"secret-k", access, map[string]int64{"score": 100})

	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
}

func TestKeyType_publishable_allowed_for_leaderboard_top(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "publishable-k", "publishable")

	var leaderboardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'main') RETURNING id`,
		tenantID, projectID).Scan(&leaderboardID))

	srv, _ := newFullStackServer(t, c)
	access := anonymousLogin(t, srv.URL, "publishable-k")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv.URL, leaderboardID),
		"publishable-k", access, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var top struct {
		Entries []json.RawMessage `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &top))
	assert.Empty(t, top.Entries)
}

func TestKeyType_publishable_allowed_for_anonymous_login(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "publishable-k", "publishable")
	srv, _ := newFullStackServer(t, c)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/anonymous", "publishable-k", nil)

	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}
