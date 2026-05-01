package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
)

// leaderboardTopTTL bounds how stale a memoised top-N reply may be. Short
// enough that a fresh score appears within a frame budget; long enough that
// hot leaderboards don't replay the same query on every request.
const leaderboardTopTTL = 10 * time.Second

// leaderboardTopCachedLimit is the only limit value that gets memoised.
// Caching every limit a caller might pass would leave us unable to
// invalidate them all on submit, so off-default reads always hit Postgres.
// 10 matches parseLimit's default and the SDK's pagination size.
const leaderboardTopCachedLimit int32 = 10

type submitScoreRequest struct {
	Score int64 `json:"score"`
}

type leaderboardEntry struct {
	EndUserID int64 `json:"end_user_id"`
	Score     int64 `json:"score"`
	Rank      int64 `json:"rank"`
}

func leaderboardTopCacheKey(tenantID, leaderboardID int64, limit int32) string {
	return fmt.Sprintf("leaderboard:top:%d:%d:%d", tenantID, leaderboardID, limit)
}

// POST /v1/leaderboards/{id}/scores — m1.md 4.3.1.
func leaderboardSubmitHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaderboardID, ok := pathInt64(r, "id")
		if !ok {
			http.Error(w, "leaderboard id required", http.StatusBadRequest)
			return
		}
		var req submitScoreRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		ctx := r.Context()
		tenantID, _ := db.TenantFromContext(ctx)
		userID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if _, err := q.GetLeaderboard(ctx, leaderboardID); err != nil {
				return err
			}
			_, err := q.SubmitScore(ctx, sqlcgen.SubmitScoreParams{
				LeaderboardID: leaderboardID, EndUserID: userID, Score: req.Score,
			})
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "leaderboard not found", http.StatusNotFound)
			return
		}
		if err != nil {
			internalError(w, "leaderboard submit: tx", err)
			return
		}

		// Invalidate the memoised top-N so the next reader pays the
		// fresh-query cost rather than serving a stale snapshot.
		// Best-effort: on Delete failure the TTL still bounds staleness.
		if d.Cache != nil {
			_ = d.Cache.Delete(ctx, leaderboardTopCacheKey(tenantID, leaderboardID, leaderboardTopCachedLimit))
		}

		w.WriteHeader(http.StatusCreated)
	}
}

// GET /v1/leaderboards/{id}/top?limit=N — m1.md 4.3.2.
func leaderboardTopHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaderboardID, ok := pathInt64(r, "id")
		if !ok {
			http.Error(w, "leaderboard id required", http.StatusBadRequest)
			return
		}
		limit := parseLimit(r.URL.Query().Get("limit"), leaderboardTopCachedLimit, 100)
		ctx := r.Context()
		tenantID, _ := db.TenantFromContext(ctx)

		cacheable := d.Cache != nil && limit == leaderboardTopCachedLimit
		cacheKey := leaderboardTopCacheKey(tenantID, leaderboardID, limit)
		if cacheable {
			if raw, err := d.Cache.Get(ctx, cacheKey); err == nil {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			}
		}

		entries, err := topFromPostgres(ctx, d, leaderboardID, limit)
		if err != nil {
			internalError(w, "leaderboard top: postgres", err)
			return
		}

		payload, err := json.Marshal(map[string]any{"entries": entries})
		if err != nil {
			internalError(w, "leaderboard top: marshal", err)
			return
		}

		if cacheable {
			// Best-effort: a Set failure just costs a re-query next call.
			_ = d.Cache.Set(ctx, cacheKey, payload, leaderboardTopTTL)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}
}

// GET /v1/leaderboards/{id}/around-me?radius=N — m1.md 4.3.3.
func leaderboardAroundMeHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leaderboardID, ok := pathInt64(r, "id")
		if !ok {
			http.Error(w, "leaderboard id required", http.StatusBadRequest)
			return
		}
		radius := parseLimit(r.URL.Query().Get("radius"), 5, 50)
		ctx := r.Context()
		userID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		entries, selfRank, err := aroundMeFromPostgres(ctx, d, leaderboardID, userID, int64(radius))
		if err != nil {
			internalError(w, "leaderboard around-me", err)
			return
		}
		writeJSON(w, map[string]any{"entries": entries, "self_rank": selfRank})
	}
}

func topFromPostgres(ctx context.Context, d Deps, leaderboardID int64, limit int32) ([]leaderboardEntry, error) {
	out := make([]leaderboardEntry, 0)
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).TopN(ctx, sqlcgen.TopNParams{
			LeaderboardID: leaderboardID, Limit: limit,
		})
		if qerr != nil {
			return qerr
		}
		for i, row := range rows {
			out = append(out, leaderboardEntry{
				EndUserID: row.EndUserID, Score: row.BestScore, Rank: int64(i),
			})
		}
		return nil
	})
	return out, err
}

func aroundMeFromPostgres(ctx context.Context, d Deps, leaderboardID, userID, radius int64) ([]leaderboardEntry, int64, error) {
	entries := make([]leaderboardEntry, 0)
	selfRank := int64(-1)

	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rank, rerr := q.LeaderboardUserRank(ctx, sqlcgen.LeaderboardUserRankParams{
			LeaderboardID: leaderboardID, EndUserID: userID,
		})
		if errors.Is(rerr, pgx.ErrNoRows) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		selfRank = rank

		low := rank - radius
		if low < 1 {
			low = 1
		}
		rows, qerr := q.LeaderboardRangeByRank(ctx, sqlcgen.LeaderboardRangeByRankParams{
			LeaderboardID: leaderboardID,
			RankLow:       low,
			RankHigh:      rank + radius,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			entries = append(entries, leaderboardEntry{
				EndUserID: row.EndUserID,
				Score:     row.BestScore,
				// Internal rank is 1-based per RANK(); convert to the
				// 0-based rank the SDK has historically seen from ZREVRANK.
				Rank: row.Rank - 1,
			})
		}
		return nil
	})
	if err != nil {
		return nil, -1, err
	}

	if selfRank > 0 {
		// Externalise as 0-based to match the historical Valkey semantics.
		selfRank--
	}
	return entries, selfRank, nil
}

func pathInt64(r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
