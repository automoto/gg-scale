//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedBoard inserts a leaderboard with explicit feature columns and returns
// its id. Named SQL params keep the option list readable at call sites.
type boardOpts struct {
	sortOrder string // "" = desc
	operator  string // "" = best
}

func seedBoard(t *testing.T, c *cluster, tenantID, projectID int64, name string, opts boardOpts) int64 {
	t.Helper()
	if opts.sortOrder == "" {
		opts.sortOrder = "desc"
	}
	if opts.operator == "" {
		opts.operator = "best"
	}
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name, sort_order, score_operator)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		tenantID, projectID, name, opts.sortOrder, opts.operator).Scan(&id))
	return id
}

func submitScore(t *testing.T, srv, key, token string, boardID int64, body any) (*http.Response, []byte) {
	t.Helper()
	return authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv, boardID), key, token, body)
}

func readTopEntries(t *testing.T, srv, key, token string, boardID int64) []map[string]any {
	t.Helper()
	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", srv, boardID), key, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Entries
}

func TestLeaderboardOperators_apply_per_board(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	cases := []struct {
		name    string
		opts    boardOpts
		submits []int64
		want    float64
	}{
		{"best_desc_keeps_highest", boardOpts{}, []int64{100, 300, 200}, 300},
		{"best_asc_keeps_lowest", boardOpts{sortOrder: "asc"}, []int64{95, 87, 90}, 87},
		{"set_replaces", boardOpts{operator: "set"}, []int64{100, 50}, 50},
		{"incr_accumulates", boardOpts{operator: "incr"}, []int64{10, 15}, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boardID := seedBoard(t, c, tenantID, projectID, tc.name, tc.opts)
			for _, s := range tc.submits {
				resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": s})
				require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
			}
			entries := readTopEntries(t, srv.URL, "k", token, boardID)
			require.Len(t, entries, 1)
			assert.Equal(t, tc.want, entries[0]["score"])
		})
	}
}

func TestLeaderboardSubmit_missing_score_is_a_validation_error(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	boardID := seedBoard(t, c, tenantID, projectID, "strict", boardOpts{})
	srv := newServerForCluster(t, c)
	token, playerID := anonymousLoginWithID(t, srv.URL, "k")

	resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]string{})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/server/leaderboards/%d/scores", srv.URL, boardID),
		"k", "", map[string]int64{"player_id": playerID})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
}

func TestLeaderboardSubmit_metadata_follows_the_standing_score(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	bestBoard := seedBoard(t, c, tenantID, projectID, "record-runs", boardOpts{})
	resp, body := submitScore(t, srv.URL, "k", token, bestBoard, map[string]any{
		"score": 300, "metadata": map[string]string{"ghost": "record"},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	// A worse run must not overwrite the record run's metadata on a 'best' board.
	resp, body = submitScore(t, srv.URL, "k", token, bestBoard, map[string]any{
		"score": 100, "metadata": map[string]string{"ghost": "worse"},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	entries := readTopEntries(t, srv.URL, "k", token, bestBoard)
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{"ghost": "record"}, entries[0]["metadata"])

	// On a 'set' board the latest submission's metadata wins.
	setBoard := seedBoard(t, c, tenantID, projectID, "loadouts", boardOpts{operator: "set"})
	for _, m := range []string{"first", "latest"} {
		resp, body = submitScore(t, srv.URL, "k", token, setBoard, map[string]any{
			"score": 10, "metadata": map[string]string{"tag": m},
		})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}
	entries = readTopEntries(t, srv.URL, "k", token, setBoard)
	require.Len(t, entries, 1)
	assert.Equal(t, map[string]any{"tag": "latest"}, entries[0]["metadata"])

	// Around-me carries metadata too.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/around-me", srv.URL, bestBoard), "k", token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var around struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &around))
	require.Len(t, around.Entries, 1)
	assert.Equal(t, map[string]any{"ghost": "record"}, around.Entries[0]["metadata"])
}

func TestLeaderboardSubmit_metadata_rejections(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	boardID := seedBoard(t, c, tenantID, projectID, "meta-strict", boardOpts{})
	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	cases := []struct {
		name string
		meta json.RawMessage
	}{
		{"array_not_object", json.RawMessage(`[1,2]`)},
		{"string_not_object", json.RawMessage(`"tag"`)},
		{"oversized", json.RawMessage(`{"pad":"` + strings.Repeat("x", 3000) + `"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]any{
				"score": 1, "metadata": tc.meta,
			})
			assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, string(body))
		})
	}

	var count int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM leaderboard_entries WHERE leaderboard_id = $1`, boardID).Scan(&count))
	assert.Zero(t, count, "rejected metadata must not write a row")
}

func TestLeaderboardSubmit_counts_attempts(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	boardID := seedBoard(t, c, tenantID, projectID, "attempts", boardOpts{})
	srv := newServerForCluster(t, c)
	token, playerID := anonymousLoginWithID(t, srv.URL, "k")

	for _, s := range []int64{10, 30, 20} {
		resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": s})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}

	var attempts int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT attempts FROM leaderboard_entries WHERE leaderboard_id = $1 AND player_id = $2`,
		boardID, playerID).Scan(&attempts))
	assert.Equal(t, 3, attempts)
}
