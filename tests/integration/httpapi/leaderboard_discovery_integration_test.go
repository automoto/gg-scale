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

func listLeaderboards(t *testing.T, srv, key, token string) []map[string]any {
	t.Helper()
	resp, body := authedReq(t, http.MethodGet, srv+"/v1/leaderboards", key, token, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Leaderboards []map[string]any `json:"leaderboards"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Leaderboards
}

func TestLeaderboardList_returns_boards_with_feature_metadata(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	seedBoard(t, c, tenantID, projectID, "alpha", boardOpts{})
	var betaID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name, sort_order, score_operator,
		    metadata, client_submissions, score_min, score_max, reset_schedule, attempt_cap,
		    current_period)
		 VALUES ($1, $2, 'beta', 'asc', 'incr', '{"icon":"gold"}', true, 0, 100000, 'weekly', 3, 2)
		 RETURNING id`,
		tenantID, projectID).Scan(&betaID))

	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	boards := listLeaderboards(t, srv.URL, "k", token)
	require.Len(t, boards, 2)

	alpha, beta := boards[0], boards[1]
	assert.Equal(t, "alpha", alpha["name"])
	assert.Equal(t, "best", alpha["score_operator"])
	assert.Equal(t, "none", alpha["reset_schedule"])
	assert.Equal(t, false, alpha["client_submissions"])
	assert.NotContains(t, alpha, "attempt_cap")
	assert.NotContains(t, alpha, "metadata")

	assert.Equal(t, "beta", beta["name"])
	assert.Equal(t, float64(betaID), beta["id"])
	assert.Equal(t, "asc", beta["sort_order"])
	assert.Equal(t, "incr", beta["score_operator"])
	assert.Equal(t, true, beta["client_submissions"])
	assert.Equal(t, float64(0), beta["score_min"])
	assert.Equal(t, float64(100000), beta["score_max"])
	assert.Equal(t, "weekly", beta["reset_schedule"])
	assert.Equal(t, float64(3), beta["attempt_cap"])
	assert.Equal(t, float64(2), beta["current_period"])
	assert.Equal(t, map[string]any{"icon": "gold"}, beta["metadata"])
}

func TestLeaderboardList_excludes_sibling_project_and_deleted_boards(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	seedBoard(t, c, tenantID, projectID, "mine", boardOpts{})

	var otherProject int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'other') RETURNING id`,
		tenantID).Scan(&otherProject))
	seedBoard(t, c, tenantID, otherProject, "sibling", boardOpts{})
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name, deleted_at) VALUES ($1, $2, 'gone', now())`,
		tenantID, projectID)
	require.NoError(t, err)

	srv := newServerForCluster(t, c)
	token, _ := anonymousLoginWithID(t, srv.URL, "k")

	boards := listLeaderboards(t, srv.URL, "k", token)
	require.Len(t, boards, 1)
	assert.Equal(t, "mine", boards[0]["name"])
}

func TestLeaderboardFriends_ranks_accepted_friends_and_caller_only(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	boardID := seedBoard(t, c, tenantID, projectID, "social", boardOpts{})
	srv := newServerForCluster(t, c)

	meTok, meID := anonymousLoginWithID(t, srv.URL, "k")
	aTok, aID := anonymousLoginWithID(t, srv.URL, "k")
	bTok, bID := anonymousLoginWithID(t, srv.URL, "k")
	strangerTok, _ := anonymousLoginWithID(t, srv.URL, "k")

	meAcc := linkPlayerAccountReturningID(t, c, meID)
	aAcc := linkPlayerAccountReturningID(t, c, aID)
	bAcc := linkPlayerAccountReturningID(t, c, bID)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE player_accounts SET display_name = 'Ace' WHERE id = $1`, aAcc)
	require.NoError(t, err)

	// Accepted edges in both directions relative to the caller; the stranger
	// has no edge at all.
	for _, edge := range [][2]string{{meAcc, aAcc}, {bAcc, meAcc}} {
		_, err := c.bootstrapPool.Exec(context.Background(),
			`INSERT INTO friend_edges (from_account_id, to_account_id, status) VALUES ($1, $2, 'accepted')`,
			edge[0], edge[1])
		require.NoError(t, err)
	}

	for _, sub := range []struct {
		tok   string
		score int64
	}{{meTok, 50}, {aTok, 100}, {bTok, 75}, {strangerTok, 999}} {
		resp, body := submitScore(t, srv.URL, "k", sub.tok, boardID, map[string]int64{"score": sub.score})
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	}

	resp, body := authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/friends", srv.URL, boardID), "k", meTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Entries []struct {
			PlayerID    int64  `json:"player_id"`
			Score       int64  `json:"score"`
			Rank        int64  `json:"rank"`
			DisplayName string `json:"display_name"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out.Entries, 3, "friends view holds the caller plus accepted friends only")

	assert.Equal(t, aID, out.Entries[0].PlayerID)
	assert.Equal(t, int64(100), out.Entries[0].Score)
	assert.Equal(t, int64(0), out.Entries[0].Rank)
	assert.Equal(t, "Ace", out.Entries[0].DisplayName)
	assert.Equal(t, bID, out.Entries[1].PlayerID)
	assert.Equal(t, int64(1), out.Entries[1].Rank)
	assert.Equal(t, meID, out.Entries[2].PlayerID)
	assert.Equal(t, int64(2), out.Entries[2].Rank)

	// An unlinked caller sees only their own entry.
	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/friends", srv.URL, boardID), "k", strangerTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out.Entries, 1)
	assert.Equal(t, int64(999), out.Entries[0].Score)
}

func TestLeaderboardFriends_caller_kept_below_a_full_page_of_friends(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 2, "k")
	boardID := seedBoard(t, c, tenantID, projectID, "crowded", boardOpts{})
	srv := newServerForCluster(t, c)

	meTok, meID := anonymousLoginWithID(t, srv.URL, "k")
	meAcc := linkPlayerAccountReturningID(t, c, meID)

	// 101 accepted friends, every one of them out-ranking the caller — one
	// more than the view's page size.
	_, err := c.bootstrapPool.Exec(context.Background(), `
WITH accs AS (
    INSERT INTO player_accounts (email, password_hash, email_verified_at)
    SELECT 'crowd-friend-' || g || '@example.com', '\x00'::bytea, now()
    FROM generate_series(1, 101) AS g
    RETURNING id
), players AS (
    INSERT INTO project_players (tenant_id, project_id, external_id, player_account_id)
    SELECT $1, $2, 'anon_crowd_' || row_number() OVER (), id FROM accs
    RETURNING id, player_account_id
), edges AS (
    INSERT INTO friend_edges (from_account_id, to_account_id, status)
    SELECT $3::uuid, player_account_id, 'accepted' FROM players
)
INSERT INTO leaderboard_entries (tenant_id, leaderboard_id, player_id, period, score)
SELECT $1, $4, id, 0, 1000 + row_number() OVER () FROM players`,
		tenantID, projectID, meAcc, boardID)
	require.NoError(t, err)

	resp, body := submitScore(t, srv.URL, "k", meTok, boardID, map[string]int64{"score": 5})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s/v1/leaderboards/%d/friends", srv.URL, boardID), "k", meTok, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Entries []struct {
			PlayerID int64 `json:"player_id"`
			Rank     int64 `json:"rank"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.Len(t, out.Entries, 101, "top page of friends plus the caller")
	last := out.Entries[len(out.Entries)-1]
	assert.Equal(t, meID, last.PlayerID, "the caller must never drop out of their own friends view")
	assert.Equal(t, int64(100), last.Rank)
}
