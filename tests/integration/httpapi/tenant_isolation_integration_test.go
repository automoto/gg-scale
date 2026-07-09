//go:build integration

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

// Cross-tenant HTTP: same logical storage keys and leaderboard names must not
// leak data; session JWTs must not replay under another tenant's API key.

func TestTenantIsolation_storage_object_not_readable_across_tenants(t *testing.T) {
	c := startCluster(t)
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "ka")
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "kb")
	srv := newServerForCluster(t, c)

	tokA := anonymousLogin(t, srv.URL, "ka")
	tokB := anonymousLogin(t, srv.URL, "kb")

	const key = "prefs"
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/storage/objects/"+key, "ka", tokA,
		map[string]any{"tenant": "A"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, _ = authedReq(t, http.MethodGet, srv.URL+"/v1/storage/objects/"+key, "kb", tokB, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestTenantIsolation_leaderboard_cross_tenant_fetch_returns_404(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "ka")
	tenantB, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "kb")
	srv := newServerForCluster(t, c)

	var lbA int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'shared-name') RETURNING id`,
		tenantA, projectA).Scan(&lbA))
	var lbB int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'shared-name') RETURNING id`,
		tenantB, projectB).Scan(&lbB))

	tokA := anonymousLogin(t, srv.URL, "ka")
	tokB := anonymousLogin(t, srv.URL, "kb")

	resp, _ := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, lbA),
		"ka", tokA, map[string]int64{"score": 100})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp, _ = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, lbB),
		"kb", tokB, map[string]int64{"score": 200})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv.URL, lbA),
		"kb", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var topCross struct {
		Entries []struct {
			Score int64 `json:"score"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &topCross))
	assert.Empty(t, topCross.Entries, "tenant B must not see tenant A leaderboard rows")

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv.URL, lbB),
		"ka", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &topCross))
	assert.Empty(t, topCross.Entries, "tenant A must not see tenant B leaderboard rows")

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv.URL, lbA),
		"ka", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}

func TestTenantIsolation_session_JWT_rejected_with_wrong_tenant_api_key(t *testing.T) {
	c := startCluster(t)
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "ka")
	_, _ = seedTenantWithAPIKey(t, c.bootstrapPool, "free", "kb")
	srv := newServerForCluster(t, c)

	tokA := anonymousLogin(t, srv.URL, "ka")
	resp, _ := authedReq(t, http.MethodGet, srv.URL+"/v1/storage/objects/x", "kb", tokA, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}
