package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
)

type submitScoreRequest struct {
	Score int64 `json:"score"`
}

type leaderboardEntry struct {
	EndUserID int64 `json:"end_user_id"`
	Score     int64 `json:"score"`
	Rank      int64 `json:"rank"`
}

func leaderboardKey(tenantID, leaderboardID int64) string {
	return fmt.Sprintf("leaderboard:%d:%d", tenantID, leaderboardID)
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

		key := leaderboardKey(tenantID, leaderboardID)
		if err := zaddBest(ctx, d.Valkey, key, userID, req.Score); err != nil {
			internalError(w, "leaderboard submit: zadd", err)
			return
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
		limit := parseLimit(r.URL.Query().Get("limit"), 10, 100)
		ctx := r.Context()
		tenantID, _ := db.TenantFromContext(ctx)

		key := leaderboardKey(tenantID, leaderboardID)
		entries, err := topFromValkey(ctx, d.Valkey, key, int64(limit))
		if err != nil {
			internalError(w, "leaderboard top: zrange", err)
			return
		}
		if len(entries) == 0 {
			// Cold cache — fall back to Postgres and rebuild ZSET opportunistically.
			entries, err = topFromPostgres(ctx, d, leaderboardID, limit)
			if err != nil {
				internalError(w, "leaderboard top: postgres", err)
				return
			}
			if err := warmCache(ctx, d.Valkey, key, entries); err != nil {
				// Cache rebuild is best-effort.
				_ = err
			}
		}

		writeJSON(w, map[string]any{"entries": entries})
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
		tenantID, _ := db.TenantFromContext(ctx)
		userID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		key := leaderboardKey(tenantID, leaderboardID)
		rank, err := d.Valkey.ZRevRank(ctx, key, strconv.FormatInt(userID, 10)).Result()
		if errors.Is(err, redis.Nil) {
			writeJSON(w, map[string]any{"entries": []leaderboardEntry{}, "self_rank": -1})
			return
		}
		if err != nil {
			internalError(w, "leaderboard around-me: zrevrank", err)
			return
		}

		start := rank - int64(radius)
		if start < 0 {
			start = 0
		}
		stop := rank + int64(radius)
		raw, err := d.Valkey.ZRevRangeWithScores(ctx, key, start, stop).Result()
		if err != nil {
			internalError(w, "leaderboard around-me: zrange", err)
			return
		}
		entries := make([]leaderboardEntry, 0, len(raw))
		for i, z := range raw {
			id, _ := strconv.ParseInt(asString(z.Member), 10, 64)
			entries = append(entries, leaderboardEntry{
				EndUserID: id, Score: int64(z.Score), Rank: start + int64(i),
			})
		}
		writeJSON(w, map[string]any{"entries": entries, "self_rank": rank})
	}
}

func zaddBest(ctx context.Context, v *redis.Client, key string, userID, score int64) error {
	// ZADD GT updates only if new score > existing — preserves "best score
	// per user" semantics in the cache.
	return v.ZAddGT(ctx, key, redis.Z{Score: float64(score), Member: strconv.FormatInt(userID, 10)}).Err()
}

func topFromValkey(ctx context.Context, v *redis.Client, key string, limit int64) ([]leaderboardEntry, error) {
	raw, err := v.ZRevRangeWithScores(ctx, key, 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]leaderboardEntry, 0, len(raw))
	for i, z := range raw {
		id, _ := strconv.ParseInt(asString(z.Member), 10, 64)
		out = append(out, leaderboardEntry{
			EndUserID: id, Score: int64(z.Score), Rank: int64(i),
		})
	}
	return out, nil
}

func topFromPostgres(ctx context.Context, d Deps, leaderboardID int64, limit int32) ([]leaderboardEntry, error) {
	var out []leaderboardEntry
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

func warmCache(ctx context.Context, v *redis.Client, key string, entries []leaderboardEntry) error {
	if len(entries) == 0 {
		return nil
	}
	zs := make([]redis.Z, len(entries))
	for i, e := range entries {
		zs[i] = redis.Z{Score: float64(e.Score), Member: strconv.FormatInt(e.EndUserID, 10)}
	}
	return v.ZAdd(ctx, key, zs...).Err()
}

func pathInt64(r *http.Request, name string) (int64, bool) {
	raw := chi.URLParam(r, name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func asString(member any) string {
	if s, ok := member.(string); ok {
		return s
	}
	return ""
}
