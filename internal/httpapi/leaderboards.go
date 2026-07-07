package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
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
	// Optional so an omitted score defaults to 0 (matches the pre-migration
	// wire); a present score of any int64 is accepted.
	Score int64 `json:"score,omitempty"`
}

type leaderboardEntry struct {
	PlayerID int64 `json:"player_id"`
	Score    int64 `json:"score"`
	Rank     int64 `json:"rank"`
}

type leaderboardSubmitInput struct {
	ID   int64 `path:"id" minimum:"1"`
	Body submitScoreRequest
}

type leaderboardTopInput struct {
	ID    int64  `path:"id" minimum:"1"`
	Limit string `query:"limit"`
}

type leaderboardTopResult struct {
	Entries []leaderboardEntry `json:"entries"`
}

type leaderboardTopOutput struct {
	Body leaderboardTopResult
}

type leaderboardAroundMeInput struct {
	ID     int64  `path:"id" minimum:"1"`
	Radius string `query:"radius"`
}

type leaderboardAroundMeResult struct {
	Entries  []leaderboardEntry `json:"entries"`
	SelfRank int64              `json:"self_rank"`
}

type leaderboardAroundMeOutput struct {
	Body leaderboardAroundMeResult
}

func leaderboardTopCacheKey(tenantID, leaderboardID int64, limit int32) string {
	return fmt.Sprintf("leaderboard:top:%d:%d:%d", tenantID, leaderboardID, limit)
}

// registerLeaderboardReadRoutes registers the player-readable top/around-me
// operations. Submit is registered separately (secret-key gated) via
// registerLeaderboardSubmit.
func registerLeaderboardReadRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "leaderboardTop",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/top",
		Summary:     "Top scores for a leaderboard",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, leaderboardTop(d))

	huma.Register(api, huma.Operation{
		OperationID: "leaderboardAroundMe",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/around-me",
		Summary:     "Scores around the caller's rank",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, leaderboardAroundMe(d))
}

// registerLeaderboardSubmit registers score submission. Score writes are
// server-authoritative: the caller must hold a secret key (enforced by the
// requireAPIKeyPermission middleware the adapter is bound behind).
func registerLeaderboardSubmit(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "submitScore",
		Method:        http.MethodPost,
		Path:          "/v1/leaderboards/{id}/scores",
		Summary:       "Submit a score to a leaderboard",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
	}, leaderboardSubmit(d))
}

func leaderboardSubmit(d Deps) func(context.Context, *leaderboardSubmitInput) (*struct{}, error) {
	return func(ctx context.Context, in *leaderboardSubmitInput) (*struct{}, error) {
		tenantID, _ := db.TenantFromContext(ctx)
		userID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if _, err := q.GetLeaderboard(ctx, in.ID); err != nil {
				return err
			}
			_, err := q.SubmitScore(ctx, sqlcgen.SubmitScoreParams{
				LeaderboardID: in.ID, PlayerID: userID, Score: in.Body.Score,
			})
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("leaderboard not found")
		}
		if err != nil {
			return nil, serverError(ctx, "leaderboard submit: tx", err)
		}

		// Invalidate the memoised top-N so the next reader pays the
		// fresh-query cost rather than serving a stale snapshot.
		// Best-effort: on Delete failure the TTL still bounds staleness.
		if d.Cache != nil {
			_ = d.Cache.Delete(ctx, leaderboardTopCacheKey(tenantID, in.ID, leaderboardTopCachedLimit))
		}

		return nil, nil
	}
}

func leaderboardTop(d Deps) func(context.Context, *leaderboardTopInput) (*leaderboardTopOutput, error) {
	return func(ctx context.Context, in *leaderboardTopInput) (*leaderboardTopOutput, error) {
		limit := parseLimit(in.Limit, leaderboardTopCachedLimit, 100)
		tenantID, _ := db.TenantFromContext(ctx)

		cacheable := d.Cache != nil && limit == leaderboardTopCachedLimit
		cacheKey := leaderboardTopCacheKey(tenantID, in.ID, limit)
		if cacheable {
			if raw, err := d.Cache.Get(ctx, cacheKey); err == nil {
				var cached leaderboardTopResult
				if json.Unmarshal(raw, &cached) == nil {
					return &leaderboardTopOutput{Body: cached}, nil
				}
			}
		}

		entries, err := topFromPostgres(ctx, d, in.ID, limit)
		if err != nil {
			return nil, serverError(ctx, "leaderboard top: postgres", err)
		}

		if cacheable {
			// Best-effort: a Set (or marshal) failure just costs a re-query.
			if payload, merr := json.Marshal(leaderboardTopResult{Entries: entries}); merr == nil {
				_ = d.Cache.Set(ctx, cacheKey, payload, leaderboardTopTTL)
			}
		}

		return &leaderboardTopOutput{Body: leaderboardTopResult{Entries: entries}}, nil
	}
}

func leaderboardAroundMe(d Deps) func(context.Context, *leaderboardAroundMeInput) (*leaderboardAroundMeOutput, error) {
	return func(ctx context.Context, in *leaderboardAroundMeInput) (*leaderboardAroundMeOutput, error) {
		radius := parseLimit(in.Radius, 5, 50)
		userID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		entries, selfRank, err := aroundMeFromPostgres(ctx, d, in.ID, userID, int64(radius))
		if err != nil {
			return nil, serverError(ctx, "leaderboard around-me", err)
		}
		return &leaderboardAroundMeOutput{Body: leaderboardAroundMeResult{Entries: entries, SelfRank: selfRank}}, nil
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
				PlayerID: row.PlayerID, Score: row.BestScore, Rank: int64(i),
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
			LeaderboardID: leaderboardID, PlayerID: userID,
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
				PlayerID: row.PlayerID,
				Score:    row.BestScore,
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
