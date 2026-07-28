//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A player pinned to project A must not read or write a leaderboard that
// belongs to project B of the SAME tenant. Leaderboards are project-scoped
// resources (created under /projects/{id}/leaderboards) and leaderboards.id is
// a single global sequence, so sibling-project ids are trivially enumerable.
func TestLeaderboard_cross_project_access_is_denied(t *testing.T) {
	c := startCluster(t)
	// Tier 2 avoids rate-limit denials; key "lb" is pinned to project A.
	tenantID, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "lb")
	ctx := context.Background()

	var projectB int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'project-b') RETURNING id`,
		tenantID).Scan(&projectB))

	var boardA, boardB int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'board-a') RETURNING id`,
		tenantID, projectA).Scan(&boardA))
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'board-b') RETURNING id`,
		tenantID, projectB).Scan(&boardB))

	// A victim player + score in project B's board, so a successful
	// cross-project read would leak it.
	var victimID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO project_players (tenant_id, project_id, external_id) VALUES ($1, $2, 'victim') RETURNING id`,
		tenantID, projectB).Scan(&victimID))
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, score, recorded_at)
		 VALUES ($1, $2, $3, 999, now())`,
		tenantID, boardB, victimID)
	require.NoError(t, err)

	srv := newServerForCluster(t, c)
	tokenA, _ := anonymousLoginWithID(t, srv.URL, "lb") // player in project A

	scoresURL := func(board int64) string { return fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, board) }
	topURL := func(board int64) string { return fmt.Sprintf("%s/v1/leaderboards/%d/top", srv.URL, board) }
	aroundURL := func(board int64) string { return fmt.Sprintf("%s/v1/leaderboards/%d/around-me", srv.URL, board) }

	// Cross-project: every player-facing op on project B's board is denied.
	resp, body := authedReq(t, http.MethodPost, scoresURL(boardB), "lb", tokenA, map[string]int64{"score": 5})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project submit must 404: %s", body)

	resp, body = authedReq(t, http.MethodGet, topURL(boardB), "lb", tokenA, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project top must 404: %s", body)

	resp, body = authedReq(t, http.MethodGet, aroundURL(boardB), "lb", tokenA, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "cross-project around-me must 404: %s", body)

	// The attacker wrote nothing to project B, and the victim's row is intact.
	var entriesInB int
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM leaderboard_entries WHERE leaderboard_id = $1`, boardB).Scan(&entriesInB))
	assert.Equal(t, 1, entriesInB, "cross-project submit must not add a row to project B's board")

	// Control: the same player operates freely on their own project's board.
	resp, body = authedReq(t, http.MethodPost, scoresURL(boardA), "lb", tokenA, map[string]int64{"score": 42})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, "same-project submit must succeed: %s", body)
	resp, body = authedReq(t, http.MethodGet, topURL(boardA), "lb", tokenA, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "same-project top must succeed: %s", body)
}
