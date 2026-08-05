package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

// leaderboardTopTTL bounds how stale a memoised top-N reply may be. Short
// enough that a fresh score appears within a frame budget; long enough that
// hot leaderboards don't replay the same query on every request. It is also
// the only bound on display_name staleness: a profile rename does not
// invalidate these entries (it would have to enumerate every board the player
// is on), it just waits out the TTL. A period reset relies on the same bound.
const leaderboardTopTTL = 10 * time.Second

// leaderboardTopCachedLimit is the only limit value that gets memoised.
// Caching every limit a caller might pass would leave us unable to
// invalidate them all on submit, so off-default reads always hit Postgres.
// 10 matches parseLimit's default and the SDK's pagination size.
const leaderboardTopCachedLimit int32 = 10

// leaderboardScoreMetadataMaxBytes caps the optional JSON object a submission
// may attach. It rides along in every read view, so it stays small.
const leaderboardScoreMetadataMaxBytes = 2048

type submitScoreRequest struct {
	Score int64 `json:"score" example:"1500"`
	// Optional JSON object stored with the score and returned in every read
	// view (replay tag, loadout, ghost reference). On a 'best' board the
	// metadata of the standing score is kept; 'set' and 'incr' keep the
	// latest submission's.
	Metadata json.RawMessage `json:"metadata,omitempty" example:"{\"ghost\":\"r-42\"}"`
}

type leaderboardEntry struct {
	PlayerID    int64           `json:"player_id" example:"42"`
	Score       int64           `json:"score" example:"1500"`
	Rank        int64           `json:"rank" example:"3"`
	DisplayName string          `json:"display_name,omitempty" example:"Nova Fox"`
	Metadata    json.RawMessage `json:"metadata,omitempty" example:"{\"ghost\":\"r-42\"}"`
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

// clientSubmitRatePerSec / clientSubmitBurst bound how fast one player can
// submit on a client-submissions board. Generous for an arcade loop (a run
// every few seconds), tight enough that flooding a board from a shipped
// binary costs real time. Server-authoritative callers are not debited.
const (
	clientSubmitRatePerSec = 10.0 / 60.0
	clientSubmitBurst      = 10.0
)

// registerLeaderboardSubmit registers score submission. Boards are
// server-authoritative by default (the caller's API key needs the leaderboard
// submit permission, i.e. a secret key); a board created with client
// submissions enabled also accepts the player's own publishable-key session,
// gated by a per-player rate limit and the board's optional score bounds.
func registerLeaderboardSubmit(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "submitScore",
		Method:      http.MethodPost,
		Path:        "/v1/leaderboards/{id}/scores",
		Summary:     "Submit a score to a leaderboard",
		Description: "Server-authoritative boards require a secret API key. Boards " +
			"with client_submissions enabled also accept the player's publishable-key " +
			"session, rate limited per player and validated against the board's " +
			"score bounds.",
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
		if err := validateScoreMetadata(in.Body.Metadata); err != nil {
			return nil, err
		}

		authoritative, err := callerMaySubmitScores(ctx, d)
		if err != nil {
			return nil, err
		}
		if !authoritative {
			// The token is debited before the board resolves, so probing for
			// boards also drains the caller's own bucket. The policy checks
			// (opt-in flag, bounds) run inside the submit transaction against
			// the primary's board row — a replica lagging behind a control
			// panel edit must never let a forbidden score through.
			tenantID, _ := db.TenantFromContext(ctx)
			key := fmt.Sprintf("ratelimit:lbsubmit:%d:%d:%d", tenantID, projectID, userID)
			if err := allowRateAction(ctx, d, key, clientSubmitRatePerSec, clientSubmitBurst); err != nil {
				return nil, err
			}
		}

		err = submitScoreToBoard(ctx, d, in.ID, projectID, scoreSubmission{
			playerID: userID, score: in.Body.Score, metadata: in.Body.Metadata,
			clientPath: !authoritative,
		}, nil)
		if werr := leaderboardSubmitError(err); werr != nil {
			return nil, werr
		}
		if err != nil {
			return nil, serverError(ctx, "leaderboard submit: tx", err)
		}
		return nil, nil
	}
}

// callerMaySubmitScores reports whether the request's API key holds the
// leaderboard submit permission — the server-authoritative path. It replaces
// the route middleware the submit route used before boards could opt in to
// client submissions, which is per-board data the router cannot see.
func callerMaySubmitScores(ctx context.Context, d Deps) (bool, error) {
	key, ok := tenant.APIKeyFromContext(ctx)
	if !ok {
		return false, huma.Error401Unauthorized("unauthorized")
	}
	if d.RBAC == nil {
		return false, huma.Error500InternalServerError("authorization unavailable")
	}
	allowed, err := d.RBAC.CanAPIKey(key, rbac.ObjectLeaderboard, rbac.ActionSubmit)
	if err != nil {
		return false, serverError(ctx, "leaderboard submit: authz", err)
	}
	return allowed, nil
}

// requireClientSubmission gates the publishable-key path against the board
// row the submit transaction read from the primary: the board must have opted
// in and the submitted value must sit inside the bounds. Checking the replica
// row instead would let a lagging replica admit a score the developer just
// forbade.
func requireClientSubmission(board sqlcgen.GetLeaderboardForSubmitRow, score int64) error {
	if !board.ClientSubmissions {
		return errClientSubmitNotAllowed
	}
	if board.ScoreMin != nil && score < *board.ScoreMin {
		return errScoreBelowMinimum
	}
	if board.ScoreMax != nil && score > *board.ScoreMax {
		return errScoreAboveMaximum
	}
	return nil
}

// errLeaderboardNotFound marks a submit against a board that does not exist in
// the caller's project. Kept distinct from pgx.ErrNoRows so a gate's
// player-miss can never be mislabeled as a board-miss.
var errLeaderboardNotFound = errors.New("leaderboard: not found")

// errAttemptCapReached marks a submission the board's attempt cap refused.
var errAttemptCapReached = errors.New("leaderboard: attempt cap reached")

// Client-path policy outcomes, raised inside the submit transaction.
var (
	errClientSubmitNotAllowed = errors.New("leaderboard: client submissions disabled")
	errScoreBelowMinimum      = errors.New("leaderboard: score below minimum")
	errScoreAboveMaximum      = errors.New("leaderboard: score above maximum")
)

// leaderboardSubmitError maps the submit sentinels onto the wire; nil means
// the error was not a submit outcome and the caller must handle it.
func leaderboardSubmitError(err error) error {
	switch {
	case errors.Is(err, errLeaderboardNotFound):
		return huma.Error404NotFound("leaderboard not found")
	case errors.Is(err, errAttemptCapReached):
		return huma.Error403Forbidden("attempt cap reached for this period")
	case errors.Is(err, errClientSubmitNotAllowed):
		return huma.Error403Forbidden("score submission is server-authoritative on this leaderboard")
	case errors.Is(err, errScoreBelowMinimum):
		return huma.Error422UnprocessableEntity("score below the leaderboard minimum")
	case errors.Is(err, errScoreAboveMaximum):
		return huma.Error422UnprocessableEntity("score above the leaderboard maximum")
	}
	return nil
}

// validateScoreMetadata accepts an absent metadata field; anything present
// must be a JSON object within the size cap. huma already rejected invalid
// JSON when it parsed the body, so the shape check is the real gate.
func validateScoreMetadata(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > leaderboardScoreMetadataMaxBytes {
		return huma.Error422UnprocessableEntity(
			fmt.Sprintf("metadata exceeds %d bytes", leaderboardScoreMetadataMaxBytes))
	}
	trimmed := bytes.TrimLeftFunc(raw, unicode.IsSpace)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(raw) {
		return huma.Error422UnprocessableEntity("metadata must be a JSON object")
	}
	return nil
}

// scoreSubmission is one score write: who, what, and the optional metadata.
// clientPath marks a publishable-key submission, which must pass the board's
// opt-in flag and score bounds inside the transaction.
type scoreSubmission struct {
	playerID   int64
	score      int64
	metadata   []byte
	clientPath bool
}

// submitScoreToBoard writes one score in a transaction and invalidates the
// memoised top-N. Shared by the player-session and server-tier submit routes,
// so the board gate and the invalidation policy cannot drift apart. gate, when
// non-nil, runs first inside the same transaction — the server tier uses it
// for its player moderation check. The board's operator, sort order, period,
// and attempt cap are read in the same transaction the upsert runs in, so a
// concurrent period reset can never split a submission across periods.
func submitScoreToBoard(ctx context.Context, d Deps, boardID, projectID int64, sub scoreSubmission, gate func(q *sqlcgen.Queries) error) error {
	tenantID, _ := db.TenantFromContext(ctx)
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if gate != nil {
			if gerr := gate(q); gerr != nil {
				return gerr
			}
		}
		// Project-scoped: a leaderboard in a sibling project resolves to no
		// rows, so the score can never land on another project's board. The
		// FOR KEY SHARE read blocks on an in-flight period reset, so the
		// period can never be one the reset job just archived.
		board, err := q.GetLeaderboardForSubmit(ctx, sqlcgen.GetLeaderboardForSubmitParams{ID: boardID, ProjectID: projectID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errLeaderboardNotFound
			}
			return err
		}
		params := sqlcgen.SubmitScoreParams{
			LeaderboardID: boardID,
			PlayerID:      sub.playerID,
			Period:        board.CurrentPeriod,
			Score:         sub.score,
			Metadata:      sub.metadata,
			ScoreOperator: board.ScoreOperator,
			SortOrder:     board.SortOrder,
			AttemptCap:    board.AttemptCap,
		}
		if sub.clientPath {
			if perr := requireClientSubmission(board, sub.score); perr != nil {
				return perr
			}
			// The request-level bounds only see the delta on an incr board;
			// the clamp bounds the accumulated total.
			params.ClampMin, params.ClampMax = board.ScoreMin, board.ScoreMax
		}
		rows, err := q.SubmitScore(ctx, params)
		if err != nil {
			return err
		}
		if rows == 0 {
			return errAttemptCapReached
		}
		return nil
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
		board, err := leaderboardInProject(ctx, d, in.ID, projectID)
		if err != nil {
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

		entries, err := topFromPostgres(ctx, d, in.ID, projectID, board.CurrentPeriod, limit)
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
		board, err := leaderboardInProject(ctx, d, in.ID, projectID)
		if err != nil {
			return nil, err
		}

		entries, selfRank, err := aroundMeFromPostgres(ctx, d, in.ID, projectID, board.CurrentPeriod, userID, int64(radius))
		if err != nil {
			return nil, serverError(ctx, "leaderboard around-me", err)
		}
		return &leaderboardAroundMeOutput{Body: leaderboardAroundMeResult{Entries: entries, SelfRank: selfRank}}, nil
	}
}

// leaderboardInProject returns the board row, or a huma 404 unless
// leaderboard id belongs to the caller's project (tenant scoping comes from
// RLS). Run this before any cached read so a sibling-project board is never
// served from cache.
func leaderboardInProject(ctx context.Context, d Deps, id, projectID int64) (sqlcgen.GetLeaderboardRow, error) {
	var board sqlcgen.GetLeaderboardRow
	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		var e error
		board, e = sqlcgen.New(tx).GetLeaderboard(ctx, sqlcgen.GetLeaderboardParams{ID: id, ProjectID: projectID})
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return board, huma.Error404NotFound("leaderboard not found")
	}
	if err != nil {
		return board, serverError(ctx, "leaderboard lookup", err)
	}
	return board, nil
}

// entryMetadata converts a stored jsonb value to the wire field (nil stays
// absent via omitempty).
func entryMetadata(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

func topFromPostgres(ctx context.Context, d Deps, leaderboardID, projectID int64, period int32, limit int32) ([]leaderboardEntry, error) {
	out := make([]leaderboardEntry, 0)
	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).TopN(ctx, sqlcgen.TopNParams{
			LeaderboardID: leaderboardID, Period: period, ProjectID: projectID, RowLimit: limit,
		})
		if qerr != nil {
			return qerr
		}
		for i, row := range rows {
			e := leaderboardEntry{
				PlayerID: row.PlayerID,
				Score:    row.Score,
				Rank:     int64(i),
				Metadata: entryMetadata(row.Metadata),
			}
			if row.DisplayName != nil {
				e.DisplayName = *row.DisplayName
			}
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

func aroundMeFromPostgres(ctx context.Context, d Deps, leaderboardID, projectID int64, period int32, userID, radius int64) ([]leaderboardEntry, int64, error) {
	entries := make([]leaderboardEntry, 0)
	selfRank := int64(-1)

	err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rank, rerr := q.LeaderboardUserRank(ctx, sqlcgen.LeaderboardUserRankParams{
			LeaderboardID: leaderboardID, Period: period, ProjectID: projectID, PlayerID: userID,
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
			Period:        period,
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
				Score:    row.Score,
				// Internal rank is 1-based per RANK(); convert to the
				// 0-based rank the SDK has historically seen from ZREVRANK.
				Rank:     row.Rank - 1,
				Metadata: entryMetadata(row.Metadata),
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
