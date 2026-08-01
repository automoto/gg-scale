package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
)

// leaderboardTopTTL bounds how stale a memoised top-N reply may be. Short
// enough that a fresh score appears within a frame budget; long enough that
// hot leaderboards don't replay the same query on every request. It is also
// the only bound on display_name staleness: a profile rename does not
// invalidate these entries (it would have to enumerate every board the player
// is on), it just waits out the TTL.
const leaderboardTopTTL = 10 * time.Second

// leaderboardTopCachedLimit is the only limit value that gets memoised.
// Caching every limit a caller might pass would leave us unable to
// invalidate them all on submit, so off-default reads always hit Postgres.
// 10 matches parseLimit's default and the SDK's pagination size.
const leaderboardTopCachedLimit int32 = 10

type submitScoreRequest struct {
	// Optional so an omitted score defaults to 0 (matches the pre-migration
	// wire); a present score of any int64 is accepted.
	Score int64 `json:"score,omitempty" example:"1500"`
}

type leaderboardEntry struct {
	PlayerID    int64  `json:"player_id" example:"42"`
	Score       int64  `json:"score" example:"1500"`
	Rank        int64  `json:"rank" example:"3"`
	DisplayName string `json:"display_name,omitempty" example:"Nova Fox"`
}

type leaderboardSubmitInput struct {
	ID   int64 `path:"id" minimum:"1" example:"1"`
	Body submitScoreRequest
}

type leaderboardTopInput struct {
	ID    int64  `path:"id" minimum:"1" example:"1"`
	Limit string `query:"limit" example:"10"`
}

type leaderboardTopResult struct {
	Entries []leaderboardEntry `json:"entries"`
}

type leaderboardTopOutput struct {
	Body leaderboardTopResult
}

type leaderboardAroundMeInput struct {
	ID     int64  `path:"id" minimum:"1" example:"1"`
	Radius string `query:"radius" example:"5"`
}

type leaderboardAroundMeResult struct {
	Entries  []leaderboardEntry `json:"entries"`
	SelfRank int64              `json:"self_rank" example:"12"`
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
		Tags:        []string{"Leaderboards"},
		Security:    playerSecurity,
	}, leaderboardTop(d))

	huma.Register(api, huma.Operation{
		OperationID: "leaderboardAroundMe",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/around-me",
		Summary:     "Scores around the caller's rank",
		Tags:        []string{"Leaderboards"},
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
		Tags:          []string{"Leaderboards"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
	}, leaderboardSubmit(d))
}

func leaderboardSubmit(d Deps) func(context.Context, *leaderboardSubmitInput) (*struct{}, error) {
	return func(ctx context.Context, in *leaderboardSubmitInput) (*struct{}, error) {
		userID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}

		err := submitScoreToBoard(ctx, d, in.ID, projectID, userID, in.Body.Score, nil)
		if errors.Is(err, errLeaderboardNotFound) {
			return nil, huma.Error404NotFound("leaderboard not found")
		}
		if err != nil {
			return nil, serverError(ctx, "leaderboard submit: tx", err)
		}
		return nil, nil
	}
}

// errLeaderboardNotFound marks a submit against a board that does not exist in
// the caller's project. Kept distinct from pgx.ErrNoRows so a gate's
// player-miss can never be mislabeled as a board-miss.
var errLeaderboardNotFound = errors.New("leaderboard: not found")

// submitScoreToBoard writes one score in a transaction and invalidates the
// memoised top-N. Shared by the player-session and server-tier submit routes,
// so the board gate and the invalidation policy cannot drift apart. gate, when
// non-nil, runs first inside the same transaction — the server tier uses it
// for its player moderation check.
func submitScoreToBoard(ctx context.Context, d Deps, boardID, projectID, playerID, score int64, gate func(q *sqlcgen.Queries) error) error {
	tenantID, _ := db.TenantFromContext(ctx)
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if gate != nil {
			if gerr := gate(q); gerr != nil {
				return gerr
			}
		}
		// Project-scoped: a leaderboard in a sibling project resolves to no
		// rows, so the score can never land on another project's board.
		if _, err := q.GetLeaderboard(ctx, sqlcgen.GetLeaderboardParams{ID: boardID, ProjectID: projectID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errLeaderboardNotFound
			}
			return err
		}
		_, err := q.SubmitScore(ctx, sqlcgen.SubmitScoreParams{
			LeaderboardID: boardID, PlayerID: playerID, Score: score,
		})
		return err
	})
	if err != nil {
		return err
	}

	// Invalidate the memoised top-N so the next reader pays the
	// fresh-query cost rather than serving a stale snapshot.
	// Best-effort: on Delete failure the TTL still bounds staleness.
	if d.Cache != nil {
		_ = d.Cache.Delete(ctx, leaderboardTopCacheKey(tenantID, boardID, leaderboardTopCachedLimit))
	}
	return nil
}

func leaderboardTop(d Deps) func(context.Context, *leaderboardTopInput) (*leaderboardTopOutput, error) {
	return func(ctx context.Context, in *leaderboardTopInput) (*leaderboardTopOutput, error) {
		limit := parseLimit(in.Limit, leaderboardTopCachedLimit, 100)
		tenantID, _ := db.TenantFromContext(ctx)
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}
		// Gate before the cache: a sibling-project board must 404, never be
		// served from a cache entry keyed only by leaderboard id.
		if err := leaderboardInProject(ctx, d, in.ID, projectID); err != nil {
			return nil, err
		}

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

		entries, err := topFromPostgres(ctx, d, in.ID, projectID, limit)
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
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}
		if err := leaderboardInProject(ctx, d, in.ID, projectID); err != nil {
			return nil, err
		}

		entries, selfRank, err := aroundMeFromPostgres(ctx, d, in.ID, projectID, userID, int64(radius))
		if err != nil {
			return nil, serverError(ctx, "leaderboard around-me", err)
		}
		return &leaderboardAroundMeOutput{Body: leaderboardAroundMeResult{Entries: entries, SelfRank: selfRank}}, nil
	}
}

// leaderboardInProject returns a huma 404 unless leaderboard id belongs to the
// caller's project (tenant scoping comes from RLS). Run this before any cached
// read so a sibling-project board is never served from cache.
func leaderboardInProject(ctx context.Context, d Deps, id, projectID int64) error {
	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		_, e := sqlcgen.New(tx).GetLeaderboard(ctx, sqlcgen.GetLeaderboardParams{ID: id, ProjectID: projectID})
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return huma.Error404NotFound("leaderboard not found")
	}
	if err != nil {
		return serverError(ctx, "leaderboard lookup", err)
	}
	return nil
}

func topFromPostgres(ctx context.Context, d Deps, leaderboardID, projectID int64, limit int32) ([]leaderboardEntry, error) {
	out := make([]leaderboardEntry, 0)
	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).TopN(ctx, sqlcgen.TopNParams{
			LeaderboardID: leaderboardID, ProjectID: projectID, RowLimit: limit,
		})
		if qerr != nil {
			return qerr
		}
		for i, row := range rows {
			e := leaderboardEntry{PlayerID: row.PlayerID, Score: row.BestScore, Rank: int64(i)}
			if row.DisplayName != nil {
				e.DisplayName = *row.DisplayName
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func aroundMeFromPostgres(ctx context.Context, d Deps, leaderboardID, projectID, userID, radius int64) ([]leaderboardEntry, int64, error) {
	entries := make([]leaderboardEntry, 0)
	selfRank := int64(-1)

	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rank, rerr := q.LeaderboardUserRank(ctx, sqlcgen.LeaderboardUserRankParams{
			LeaderboardID: leaderboardID, ProjectID: projectID, PlayerID: userID,
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
			ProjectID:     projectID,
			RankLow:       low,
			RankHigh:      rank + radius,
		})
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			e := leaderboardEntry{
				PlayerID: row.PlayerID,
				Score:    row.BestScore,
				// Internal rank is 1-based per RANK(); convert to the
				// 0-based rank the SDK has historically seen from ZREVRANK.
				Rank: row.Rank - 1,
			}
			if row.DisplayName != nil {
				e.DisplayName = *row.DisplayName
			}
			entries = append(entries, e)
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
