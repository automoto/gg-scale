package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
)

type leaderboardPeriodsInput struct {
	ID     int64  `path:"id" minimum:"1" example:"1"`
	Limit  string `query:"limit" example:"50"`
	Cursor string `query:"cursor" example:"12"`
}

type leaderboardPeriodSummary struct {
	Period    int32     `json:"period" example:"12"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

type leaderboardPeriodsResult struct {
	CurrentPeriod   int32                      `json:"current_period" example:"13"`
	ResetSchedule   string                     `json:"reset_schedule" enum:"none,daily,weekly,monthly" example:"weekly"`
	PeriodStartedAt *time.Time                 `json:"period_started_at,omitempty"`
	NextResetAt     *time.Time                 `json:"next_reset_at,omitempty"`
	Periods         []leaderboardPeriodSummary `json:"periods"`
	NextCursor      string                     `json:"next_cursor,omitempty" example:"7"`
}

type leaderboardPeriodsOutput struct {
	Body leaderboardPeriodsResult
}

type leaderboardPeriodTopInput struct {
	ID int64 `path:"id" minimum:"1" example:"1"`
	// int32 matches the column type; a number beyond it cannot be a period.
	Period int32  `path:"period" minimum:"0" example:"12"`
	Limit  string `query:"limit" example:"10"`
}

// registerLeaderboardPeriodRoutes registers the period history reads: the
// finished-period list and past-period top entries.
func registerLeaderboardPeriodRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "listLeaderboardPeriods",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/periods",
		Summary:     "Reset history of a leaderboard",
		Description: "The board's current period and its finished periods, newest " +
			"first, keyset-paginated on the period number. Entries of a finished " +
			"period stay readable via the period top route.",
		Tags:     []string{"Leaderboards"},
		Security: playerSecurity,
	}, leaderboardPeriods(d))

	huma.Register(api, huma.Operation{
		OperationID: "leaderboardPeriodTop",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/periods/{period}/top",
		Summary:     "Top scores of one leaderboard period",
		Description: "Like the top route, for any period up to and including the " +
			"current one. A period the board has not reached is 404.",
		Tags:     []string{"Leaderboards"},
		Security: playerSecurity,
	}, leaderboardPeriodTop(d))
}

func leaderboardPeriods(d Deps) func(context.Context, *leaderboardPeriodsInput) (*leaderboardPeriodsOutput, error) {
	return func(ctx context.Context, in *leaderboardPeriodsInput) (*leaderboardPeriodsOutput, error) {
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}
		board, err := leaderboardInProject(ctx, d, in.ID, projectID)
		if err != nil {
			return nil, err
		}

		limit := parseLimit(in.Limit, 50, 200)
		// The cursor is exclusive-descending; the default covers every
		// finished period (all are below the current one).
		before := board.CurrentPeriod
		if c, cerr := strconv.ParseInt(in.Cursor, 10, 32); cerr == nil {
			before = int32(c)
		}

		result := leaderboardPeriodsResult{
			CurrentPeriod: board.CurrentPeriod,
			ResetSchedule: board.ResetSchedule,
			Periods:       make([]leaderboardPeriodSummary, 0),
		}
		result.PeriodStartedAt = timestamptzPtr(board.PeriodStartedAt)
		result.NextResetAt = timestamptzPtr(board.NextResetAt)

		err = d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			// Probe one row beyond the page so next_cursor is only set when
			// a next page truly exists.
			rows, qerr := sqlcgen.New(tx).ListLeaderboardPeriods(ctx, sqlcgen.ListLeaderboardPeriodsParams{
				LeaderboardID: in.ID,
				ProjectID:     projectID,
				BeforePeriod:  before,
				RowLimit:      limit + 1,
			})
			if qerr != nil {
				return qerr
			}
			if len(rows) > int(limit) {
				rows = rows[:limit]
				result.NextCursor = strconv.FormatInt(int64(rows[len(rows)-1].Period), 10)
			}
			for _, row := range rows {
				result.Periods = append(result.Periods, leaderboardPeriodSummary{
					Period:    row.Period,
					StartedAt: row.StartedAt.Time,
					EndedAt:   row.EndedAt.Time,
				})
			}
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "leaderboard periods", err)
		}
		return &leaderboardPeriodsOutput{Body: result}, nil
	}
}

func leaderboardPeriodTop(d Deps) func(context.Context, *leaderboardPeriodTopInput) (*leaderboardTopOutput, error) {
	return func(ctx context.Context, in *leaderboardPeriodTopInput) (*leaderboardTopOutput, error) {
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}
		board, err := leaderboardInProject(ctx, d, in.ID, projectID)
		if err != nil {
			return nil, err
		}
		if in.Period > board.CurrentPeriod {
			return nil, huma.Error404NotFound("period not found")
		}

		// Past periods are immutable and rarely read — no memoisation, unlike
		// the current-period top route.
		limit := parseLimit(in.Limit, leaderboardTopCachedLimit, 100)
		entries, err := topFromPostgres(ctx, d, in.ID, projectID, in.Period, limit)
		if err != nil {
			return nil, serverError(ctx, "leaderboard period top", err)
		}
		return &leaderboardTopOutput{Body: leaderboardTopResult{Entries: entries}}, nil
	}
}
