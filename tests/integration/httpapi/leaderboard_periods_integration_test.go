//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type periodsResponse struct {
	CurrentPeriod int32  `json:"current_period"`
	ResetSchedule string `json:"reset_schedule"`
	NextResetAt   string `json:"next_reset_at"`
	Periods       []struct {
		Period    int32  `json:"period"`
		StartedAt string `json:"started_at"`
		EndedAt   string `json:"ended_at"`
	} `json:"periods"`
	NextCursor string `json:"next_cursor"`
}

func getPeriods(t *testing.T, srv, key, token string, boardID int64, query string) periodsResponse {
	t.Helper()
	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods%s", srv, boardID, query), key, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out periodsResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestLeaderboardPeriods_history_and_past_period_reads(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour)
	var boardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO leaderboards (tenant_id, project_id, name, reset_schedule, current_period, period_started_at, next_reset_at)
		 VALUES ($1, $2, 'weekly-cup', 'weekly', 2, $3, $4) RETURNING id`,
		tenantID, projectID, now.AddDate(0, 0, -3), now.AddDate(0, 0, 4)).Scan(&boardID))
	for p := range 2 {
		_, err := c.bootstrapPool.Exec(ctx,
			`INSERT INTO leaderboard_periods (tenant_id, leaderboard_id, period, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, boardID, p,
			now.AddDate(0, 0, -7*(3-p)), now.AddDate(0, 0, -7*(2-p)))
		require.NoError(t, err)
	}

	srv := newServerForCluster(t, c)
	token, playerID := anonymousLoginWithID(t, srv.URL, "k")

	// A row in the finished period 1, seeded directly (the API can only
	// write into the current period).
	_, err := c.bootstrapPool.Exec(ctx,
		`INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, period, score)
		 VALUES ($1, $2, $3, 1, 500)`, tenantID, boardID, playerID)
	require.NoError(t, err)
	// The current period's score arrives through the API.
	resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": 100})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	list := getPeriods(t, srv.URL, "k", token, boardID, "")
	assert.Equal(t, int32(2), list.CurrentPeriod)
	assert.Equal(t, "weekly", list.ResetSchedule)
	assert.NotEmpty(t, list.NextResetAt)
	require.Len(t, list.Periods, 2)
	assert.Equal(t, int32(1), list.Periods[0].Period, "newest finished period first")
	assert.Equal(t, int32(0), list.Periods[1].Period)
	assert.NotEmpty(t, list.Periods[0].StartedAt)
	assert.NotEmpty(t, list.Periods[0].EndedAt)
	assert.Empty(t, list.NextCursor, "a final page carries no cursor")

	// Keyset pagination: one per page, cursor walks backwards.
	page1 := getPeriods(t, srv.URL, "k", token, boardID, "?limit=1")
	require.Len(t, page1.Periods, 1)
	assert.Equal(t, int32(1), page1.Periods[0].Period)
	require.NotEmpty(t, page1.NextCursor)
	page2 := getPeriods(t, srv.URL, "k", token, boardID, "?limit=1&cursor="+page1.NextCursor)
	require.Len(t, page2.Periods, 1)
	assert.Equal(t, int32(0), page2.Periods[0].Period)
	assert.Empty(t, page2.NextCursor)

	// Past-period top reads the archived entries; the current period reads
	// the live ones; a future period does not exist.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods/1/top", srv.URL, boardID), "k", token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var top struct {
		Entries []struct {
			Score int64 `json:"score"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &top))
	require.Len(t, top.Entries, 1)
	assert.Equal(t, int64(500), top.Entries[0].Score)

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods/2/top", srv.URL, boardID), "k", token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &top))
	require.Len(t, top.Entries, 1)
	assert.Equal(t, int64(100), top.Entries[0].Score)

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods/3/top", srv.URL, boardID), "k", token, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestLeaderboardPeriods_are_project_scoped(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	var otherProject int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'other') RETURNING id`,
		tenantID).Scan(&otherProject))
	foreignBoard := seedBoard(t, c, tenantID, otherProject, "foreign", boardOpts{})

	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods", srv.URL, foreignBoard), "k", token, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/periods/0/top", srv.URL, foreignBoard), "k", token, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestAttemptCap_blocks_over_cap_and_resets_with_the_period(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	ctx := context.Background()
	var boardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`INSERT INTO leaderboards (tenant_id, project_id, name, attempt_cap) VALUES ($1, $2, 'capped', 2) RETURNING id`,
		tenantID, projectID).Scan(&boardID))

	srv := newServerForCluster(t, c)
	token, playerID := anonymousLoginWithID(t, srv.URL, "k")

	for _, s := range []int64{10, 30} {
		resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": s})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}
	resp, body := submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": 99})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "attempt cap")

	// The server-tier route obeys the same cap.
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/server/leaderboards/%d/scores", srv.URL, boardID),
		"k", "", map[string]int64{"player_id": playerID, "score": 99})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	var score, attempts int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT score, attempts FROM leaderboard_entries WHERE leaderboard_id = $1 AND player_id = $2`,
		boardID, playerID).Scan(&score, &attempts))
	assert.Equal(t, int64(30), score, "a capped submission must not change the score")
	assert.Equal(t, int64(2), attempts)

	// A new period grants a fresh attempt budget.
	_, err := c.bootstrapPool.Exec(ctx,
		`UPDATE leaderboards SET current_period = current_period + 1 WHERE id = $1`, boardID)
	require.NoError(t, err)
	resp, body = submitScore(t, srv.URL, "k", token, boardID, map[string]int64{"score": 7})
	assert.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
}
