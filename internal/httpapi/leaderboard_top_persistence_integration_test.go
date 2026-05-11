//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

// Many score submissions from distinct end-users produce a stable top-N from
// Postgres; a new app instance with an empty cache must return the same ordering.

func TestLeaderboard_hundred_scores_from_ten_users_top_order_survives_fresh_app_cache(t *testing.T) {
	c := startCluster(t)
	// Premium tier avoids rate-limit denials while posting 100 scores in quick succession.
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "premium", "k")

	var leaderboardID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'bulk-top') RETURNING id`,
		tenantID, projectID).Scan(&leaderboardID))

	srv := newServerForCluster(t, c)
	tokens := make([]string, 10)
	ids := make([]int64, 10)
	for i := range 10 {
		tokens[i], ids[i] = anonymousLoginWithID(t, srv.URL, "k")
	}

	for userIdx := range 10 {
		for s := range 10 {
			score := int64(userIdx*10 + s)
			resp, _ := authedReq(t, http.MethodPost,
				fmt.Sprintf("%s/v1/leaderboards/%d/scores", srv.URL, leaderboardID),
				"k", tokens[userIdx], map[string]int64{"score": score})
			require.Equal(t, http.StatusCreated, resp.StatusCode)
		}
	}

	want := make([]struct {
		endUserID int64
		best      int64
	}, 10)
	for i := range 10 {
		want[i].endUserID = ids[i]
		want[i].best = int64(i*10 + 9)
	}

	readTop := func(base string) []leaderboardTopEntry {
		t.Helper()
		resp, body := authedReq(t, http.MethodGet,
			fmt.Sprintf("%s/v1/leaderboards/%d/top?limit=10", base, leaderboardID),
			"k", tokens[0], nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var out struct {
			Entries []leaderboardTopEntry `json:"entries"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		return out.Entries
	}

	entries := readTop(srv.URL)
	require.Len(t, entries, 10)
	for i := range 10 {
		assert.Equal(t, want[9-i].endUserID, entries[i].EndUserID)
		assert.Equal(t, want[9-i].best, entries[i].Score)
	}

	freshCache := memory.New()
	t.Cleanup(func() { _ = freshCache.Close(context.Background()) })
	c.cache = freshCache

	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test",
		Pool:    db.NewPool(c.appPool),
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer:  signer,
		Mailer:  &mailer.Recorder{},
		Cache:   c.cache,
		Fleet:   fleet.NewRegistry(30 * time.Second),
	})
	srv2 := httptest.NewServer(router)
	t.Cleanup(srv2.Close)

	entriesAfter := readTop(srv2.URL)
	require.Len(t, entriesAfter, 10)
	assert.Equal(t, entries, entriesAfter)
}

type leaderboardTopEntry struct {
	EndUserID int64 `json:"end_user_id"`
	Score     int64 `json:"score"`
	Rank      int64 `json:"rank"`
}
