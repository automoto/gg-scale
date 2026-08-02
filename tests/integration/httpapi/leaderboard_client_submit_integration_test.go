//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedClientBoard inserts a board with client submissions enabled and
// optional score bounds (nil = unbounded).
func seedClientBoard(t *testing.T, c *cluster, tenantID, projectID int64, name string, scoreMin, scoreMax *int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name, client_submissions, score_min, score_max)
		 VALUES ($1, $2, $3, true, $4, $5) RETURNING id`,
		tenantID, projectID, name, scoreMin, scoreMax).Scan(&id))
	return id
}

func TestClientSubmit_publishable_key_allowed_only_on_opted_in_boards(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "pub-k", "publishable")
	openBoard := seedClientBoard(t, c, tenantID, projectID, "arcade", nil, nil)
	defaultBoard := seedBoard(t, c, tenantID, projectID, "ranked", boardOpts{})

	srv, _ := newFullStackServer(t, c)
	token, playerID := anonymousLoginWithID(t, srv.URL, "pub-k")

	resp, body := submitScore(t, srv.URL, "pub-k", token, openBoard, map[string]int64{"score": 4200})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	entries := readTopEntries(t, srv.URL, "pub-k", token, openBoard)
	require.Len(t, entries, 1)
	assert.Equal(t, float64(playerID), entries[0]["player_id"])
	assert.Equal(t, float64(4200), entries[0]["score"])

	resp, body = submitScore(t, srv.URL, "pub-k", token, defaultBoard, map[string]int64{"score": 1})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func TestClientSubmit_score_bounds_gate_clients_not_servers(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "pub-k", "publishable")
	low, high := int64(0), int64(100)
	boardID := seedClientBoard(t, c, tenantID, projectID, "bounded", &low, &high)

	srv, _ := newFullStackServer(t, c)
	pubToken, _ := anonymousLoginWithID(t, srv.URL, "pub-k")
	secretToken, _ := anonymousLoginWithID(t, srv.URL, "secret-k")

	resp, body := submitScore(t, srv.URL, "pub-k", pubToken, boardID, map[string]int64{"score": -5})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
	resp, body = submitScore(t, srv.URL, "pub-k", pubToken, boardID, map[string]int64{"score": 500})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
	resp, body = submitScore(t, srv.URL, "pub-k", pubToken, boardID, map[string]int64{"score": 50})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	// A trusted server-authoritative caller is not bounded.
	resp, body = submitScore(t, srv.URL, "secret-k", secretToken, boardID, map[string]int64{"score": 500})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
}

func TestClientSubmit_incr_total_clamps_to_bounds_for_clients_only(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "pub-k", "publishable")
	var boardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name, score_operator, client_submissions, score_min, score_max)
		 VALUES ($1, $2, 'xp', 'incr', true, 0, 1000) RETURNING id`,
		tenantID, projectID).Scan(&boardID))

	srv, _ := newFullStackServer(t, c)
	pubToken, playerID := anonymousLoginWithID(t, srv.URL, "pub-k")

	// Each delta is inside the bounds, but the accumulated total saturates
	// at score_max instead of stacking past it.
	for range 2 {
		resp, body := submitScore(t, srv.URL, "pub-k", pubToken, boardID, map[string]int64{"score": 800})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}
	var total int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT score FROM leaderboard_entries WHERE leaderboard_id = $1 AND player_id = $2`,
		boardID, playerID).Scan(&total))
	assert.Equal(t, int64(1000), total, "client incr totals saturate at score_max")

	// A trusted server-authoritative caller stays unclamped.
	secretToken, secretPlayer := anonymousLoginWithID(t, srv.URL, "secret-k")
	for range 2 {
		resp, body := submitScore(t, srv.URL, "secret-k", secretToken, boardID, map[string]int64{"score": 800})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT score FROM leaderboard_entries WHERE leaderboard_id = $1 AND player_id = $2`,
		boardID, secretPlayer).Scan(&total))
	assert.Equal(t, int64(1600), total)
}

func TestClientSubmit_per_player_rate_limit(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "secret-k")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "pub-k", "publishable")
	boardID := seedClientBoard(t, c, tenantID, projectID, "spammable", nil, nil)

	srv, _ := newFullStackServer(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "pub-k")
	otherToken, _ := anonymousLoginWithID(t, srv.URL, "pub-k")

	for i := range 10 {
		resp, body := submitScore(t, srv.URL, "pub-k", token, boardID, map[string]int64{"score": int64(i)})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}
	resp, body := submitScore(t, srv.URL, "pub-k", token, boardID, map[string]int64{"score": 11})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))

	// The bucket is per player: a different player still submits fine.
	resp, body = submitScore(t, srv.URL, "pub-k", otherToken, boardID, map[string]int64{"score": 1})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
}
