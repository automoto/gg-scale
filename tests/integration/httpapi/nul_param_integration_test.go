//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/rbac"
)

// A NUL byte in a path or query value must never reach Postgres as text; it
// fails there with SQLSTATE 22021 and used to surface as a 500.
func TestNULParams_should_not_return_500(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "nul")
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason)
		 VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureP2PRelay))
	require.NoError(t, err)
	srv := newRelayServerForCluster(t, c)
	tok := anonymousLogin(t, srv.URL, "nul")

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/game-session/gs_%00"},
		{http.MethodPost, "/v1/game-session/gs_%00/join"},
		{http.MethodPost, "/v1/game-session/gs_%00/heartbeat"},
		{http.MethodDelete, "/v1/game-session/gs_%00"},
		{http.MethodGet, "/v1/game-session/gs_%00/signals"},
		{http.MethodPost, "/v1/game-session/gs_%00/signals"},
		{http.MethodGet, "/v1/game-sessions?title_id=a%00b"},
		{http.MethodGet, "/v1/game-sessions?cursor=gs_%00"},
		{http.MethodGet, "/v1/game-session?joinCode=AB%00CD"},
		{http.MethodPost, "/v1/relay/credentials?match_id=mm_%00"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp, body := authedReq(t, tc.method, srv.URL+tc.path, "nul", tok, map[string]any{})
			assert.Less(t, resp.StatusCode, http.StatusInternalServerError, string(body))
		})
	}
}
