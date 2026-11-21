package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
)

// timestamptzPtr converts a nullable DB timestamp to the wire shape: the
// address of the time when set, nil (omitted via omitempty) otherwise.
func timestamptzPtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// leaderboardFriendsMaxEntries bounds the friends view; a player with more
// scored friends than this sees the best-ranked slice.
const leaderboardFriendsMaxEntries = 100

// leaderboardListPageSize is the default and the largest page of the board
// list. A board can have 16 KiB of metadata, so one page is at most about
// 1.6 MB.
const leaderboardListPageSize = 100

type leaderboardInfo struct {
	ID                int64           `json:"id" example:"1"`
	Name              string          `json:"name" example:"weekly-high-score"`
	SortOrder         string          `json:"sort_order" enum:"asc,desc" example:"desc"`
	ScoreOperator     string          `json:"score_operator" enum:"best,set,incr" example:"best"`
	ClientSubmissions bool            `json:"client_submissions" example:"false"`
	ScoreMin          *int64          `json:"score_min,omitempty" example:"0"`
	ScoreMax          *int64          `json:"score_max,omitempty" example:"1000000"`
	AttemptCap        *int32          `json:"attempt_cap,omitempty" example:"3"`
	ResetSchedule     string          `json:"reset_schedule" enum:"none,daily,weekly,monthly" example:"weekly"`
	CurrentPeriod     int32           `json:"current_period" example:"2"`
	PeriodStartedAt   *time.Time      `json:"period_started_at,omitempty"`
	NextResetAt       *time.Time      `json:"next_reset_at,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty" example:"{\"icon\":\"gold\"}"`
}

type leaderboardListInput struct {
	Limit  string `query:"limit" example:"100"`
	Cursor string `query:"cursor" example:"weekly-high-score"`
}

type leaderboardListResult struct {
	Leaderboards []leaderboardInfo `json:"leaderboards"`
	// NextCursor is the name of the last board of the page. It is set only
	// when there is a next page.
	NextCursor string `json:"next_cursor,omitempty" example:"weekly-high-score"`
}

type leaderboardListOutput struct {
	Body leaderboardListResult
}

type leaderboardFriendsInput struct {
	ID int64 `path:"id" minimum:"1" example:"1"`
}

// registerLeaderboardDiscoveryRoutes registers the runtime discovery reads:
// the board list and the friends-scoped view. Registered alongside the other
// player-readable leaderboard routes.
func registerLeaderboardDiscoveryRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "listLeaderboards",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards",
		Summary:     "List the project's leaderboards",
		Description: "Runtime discovery: board ids, sort order, score operator, period " +
			"state, submission policy, and the developer-defined metadata blob — no " +
			"hardcoded ids needed in the client. Sorted by name and paged: limit is " +
			"at most 100 (the default); send next_cursor as cursor for the next page.",
		Tags:     []string{"Leaderboards"},
		Security: playerSecurity,
	}, leaderboardList(d))

	huma.Register(api, huma.Operation{
		OperationID: "leaderboardFriends",
		Method:      http.MethodGet,
		Path:        "/v1/leaderboards/{id}/friends",
		Summary:     "Scores of the caller and their accepted friends",
		Description: "Current-period entries for the caller plus accepted friends who " +
			"are players in this project, ranked within that group (0-based). The " +
			"caller is always included when they have a score, even below a full " +
			"page of higher-ranked friends. An unlinked caller sees only their own " +
			"entry.",
		Tags:     []string{"Leaderboards"},
		Security: playerSecurity,
	}, leaderboardFriends(d))
}

func leaderboardList(d Deps) func(context.Context, *leaderboardListInput) (*leaderboardListOutput, error) {
	return func(ctx context.Context, in *leaderboardListInput) (*leaderboardListOutput, error) {
		projectID, ok := playerauth.ProjectIDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player project")
		}
		limit := parseLimit(in.Limit, leaderboardListPageSize, leaderboardListPageSize)
		result := leaderboardListResult{Leaderboards: make([]leaderboardInfo, 0)}
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			// Get one row more than the page, so next_cursor is set only when
			// a next page exists.
			rows, qerr := sqlcgen.New(tx).ListLeaderboardsPageForProject(ctx, sqlcgen.ListLeaderboardsPageForProjectParams{
				ProjectID: projectID,
				AfterName: in.Cursor,
				RowLimit:  limit + 1,
			})
			if qerr != nil {
				return qerr
			}
			if len(rows) > int(limit) {
				rows = rows[:limit]
				result.NextCursor = rows[len(rows)-1].Name
			}
			for _, row := range rows {
				result.Leaderboards = append(result.Leaderboards, leaderboardInfo{
					ID:                row.ID,
					Name:              row.Name,
					SortOrder:         row.SortOrder,
					ScoreOperator:     row.ScoreOperator,
					ClientSubmissions: row.ClientSubmissions,
					ScoreMin:          row.ScoreMin,
					ScoreMax:          row.ScoreMax,
					AttemptCap:        row.AttemptCap,
					ResetSchedule:     row.ResetSchedule,
					CurrentPeriod:     row.CurrentPeriod,
					PeriodStartedAt:   timestamptzPtr(row.PeriodStartedAt),
					NextResetAt:       timestamptzPtr(row.NextResetAt),
					Metadata:          entryMetadata(row.Metadata),
				})
			}
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "leaderboard list", err)
		}
		return &leaderboardListOutput{Body: result}, nil
	}
}

func leaderboardFriends(d Deps) func(context.Context, *leaderboardFriendsInput) (*leaderboardTopOutput, error) {
	return func(ctx context.Context, in *leaderboardFriendsInput) (*leaderboardTopOutput, error) {
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

		entries := make([]leaderboardEntry, 0)
		err = d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			playerIDs, qerr := friendPlayerIDs(ctx, q, userID, projectID)
			if qerr != nil {
				return qerr
			}
			rows, qerr := q.LeaderboardEntriesForPlayers(ctx, sqlcgen.LeaderboardEntriesForPlayersParams{
				LeaderboardID: in.ID,
				Period:        board.CurrentPeriod,
				ProjectID:     projectID,
				PlayerIds:     append(playerIDs, userID),
				RowLimit:      leaderboardFriendsMaxEntries,
			})
			if qerr != nil {
				return qerr
			}
			callerListed := false
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
				if row.PlayerID == userID {
					callerListed = true
				}
				entries = append(entries, e)
			}
			if callerListed || len(rows) < leaderboardFriendsMaxEntries {
				return nil
			}
			// The size cap cut the caller out (out-ranked by a full page of
			// friends); the contract is caller-plus-friends, so fetch their
			// row and append it below the page.
			self, qerr := q.LeaderboardEntriesForPlayers(ctx, sqlcgen.LeaderboardEntriesForPlayersParams{
				LeaderboardID: in.ID,
				Period:        board.CurrentPeriod,
				ProjectID:     projectID,
				PlayerIds:     []int64{userID},
				RowLimit:      1,
			})
			if qerr != nil || len(self) == 0 {
				return qerr
			}
			e := leaderboardEntry{
				PlayerID: self[0].PlayerID,
				Score:    self[0].Score,
				Rank:     int64(len(entries)),
				Metadata: entryMetadata(self[0].Metadata),
			}
			if self[0].DisplayName != nil {
				e.DisplayName = *self[0].DisplayName
			}
			entries = append(entries, e)
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "leaderboard friends", err)
		}
		return &leaderboardTopOutput{Body: leaderboardTopResult{Entries: entries}}, nil
	}
}

// friendPlayerIDs resolves the caller's accepted friends to player ids in the
// caller's project. An unlinked caller has no friends by definition — the
// friend graph hangs off the global account.
func friendPlayerIDs(ctx context.Context, q *sqlcgen.Queries, userID, projectID int64) ([]int64, error) {
	acc, err := q.GetPlayerLinkedAccountID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !acc.Valid {
		return nil, nil
	}
	accountIDs, err := q.ListAcceptedFriendAccountIDs(ctx, acc)
	if err != nil || len(accountIDs) == 0 {
		return nil, err
	}
	rows, err := q.ResolvePlayersForAccountsInProject(ctx, sqlcgen.ResolvePlayersForAccountsInProjectParams{
		ProjectID:  projectID,
		AccountIds: accountIDs,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.PlayerID)
	}
	return ids, nil
}
