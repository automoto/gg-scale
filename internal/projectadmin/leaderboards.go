package projectadmin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/period"
)

var (
	// ErrDuplicateLeaderboard means a live board in the project has the name.
	ErrDuplicateLeaderboard = errors.New("projectadmin: leaderboard with that name already exists")
	// ErrSortOrderLocked rejects a sort-order change on a board that already
	// has entries: collapsed bests are frozen under the order they were
	// written with.
	ErrSortOrderLocked = errors.New("projectadmin: sort order is fixed once scores exist")
)

// LeaderboardSettings are the editable leaderboard settings. They are also
// the revision snapshot. ScoreOperator is fixed at creation; an update
// ignores it.
type LeaderboardSettings struct {
	Name              string          `json:"name"`
	SortOrder         string          `json:"sort_order"`
	ScoreOperator     string          `json:"score_operator"`
	ClientSubmissions bool            `json:"client_submissions"`
	ScoreMin          *int64          `json:"score_min,omitempty"`
	ScoreMax          *int64          `json:"score_max,omitempty"`
	ResetSchedule     string          `json:"reset_schedule"`
	AttemptCap        *int32          `json:"attempt_cap,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
}

func (s LeaderboardSettings) auditPayload() map[string]any {
	p := map[string]any{
		"leaderboard_name":   s.Name,
		"reset_schedule":     s.ResetSchedule,
		"client_submissions": s.ClientSubmissions,
		"score_operator":     s.ScoreOperator,
	}
	if s.AttemptCap != nil {
		p["attempt_cap"] = *s.AttemptCap
	}
	return p
}

// CreateLeaderboard creates a board and records revision 1.
func CreateLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID int64, s LeaderboardSettings, actor Actor) (int64, error) {
	params := sqlcgen.CreateLeaderboardParams{
		ProjectID:         projectID,
		Name:              s.Name,
		SortOrder:         s.SortOrder,
		ScoreOperator:     s.ScoreOperator,
		Metadata:          s.Metadata,
		ClientSubmissions: s.ClientSubmissions,
		ScoreMin:          s.ScoreMin,
		ScoreMax:          s.ScoreMax,
		ResetSchedule:     s.ResetSchedule,
		AttemptCap:        s.AttemptCap,
	}
	// One clock for both fields: a second Now() straddling a calendar
	// boundary would put next_reset_at before period_started_at.
	now := time.Now().UTC()
	if next, ok := period.NextReset(s.ResetSchedule, now); ok {
		params.PeriodStartedAt = pgtype.Timestamptz{Time: now, Valid: true}
		params.NextResetAt = pgtype.Timestamptz{Time: next, Valid: true}
	}
	snapshot, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	var id int64
	ctx = db.WithTenant(ctx, tenantID)
	err = pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if id, err = q.CreateLeaderboard(ctx, params); err != nil {
			return translateDuplicate(err)
		}
		w := revisionWrite{
			projectID: projectID, kind: KindLeaderboard, resourceID: id,
			after: snapshot, actor: actor, source: actor.source(),
		}
		if _, err := recordRevision(ctx, q, 0, w); err != nil {
			return err
		}
		return actor.audit(ctx, tx, tenantID, projectID, "leaderboard.create", strconv.FormatInt(id, 10), s.auditPayload())
	})
	return id, err
}

// UpdateLeaderboard saves settings onto a live board and records a revision.
// It returns pgx.ErrNoRows when the board does not exist or is deleted.
func UpdateLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, s LeaderboardSettings, expected *int64, actor Actor) (int64, error) {
	return writeLeaderboard(ctx, pool, tenantID, projectID, id, expected, actor, actor.source(), "leaderboard.update",
		func(*sqlcgen.Queries) (LeaderboardSettings, error) { return s, nil })
}

// RollbackLeaderboard applies the settings of a kept revision as a new
// revision. Scores are not restored.
func RollbackLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID, id, revision int64, expected *int64, actor Actor) (int64, error) {
	return writeLeaderboard(ctx, pool, tenantID, projectID, id, expected, actor, sourceRollback, "leaderboard.rollback",
		func(q *sqlcgen.Queries) (LeaderboardSettings, error) {
			var s LeaderboardSettings
			snapshot, err := loadRevision(ctx, q, projectID, KindLeaderboard, id, revision)
			if err != nil {
				return s, err
			}
			return s, json.Unmarshal(snapshot, &s)
		})
}

// writeLeaderboard is the shared update path. Period bookkeeping only moves
// when the schedule changes: recomputing next_reset_at on an unrelated edit
// would erase an overdue reset the job has not caught up with yet.
func writeLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, expected *int64, actor Actor, source, action string, value func(*sqlcgen.Queries) (LeaderboardSettings, error)) (int64, error) {
	var rev int64
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		cur, err := q.GetLeaderboardForUpdate(ctx, sqlcgen.GetLeaderboardForUpdateParams{ProjectID: projectID, ID: id})
		if err != nil {
			return err
		}
		before, err := json.Marshal(LeaderboardSettings{
			Name: cur.Name, SortOrder: cur.SortOrder, ScoreOperator: cur.ScoreOperator,
			ClientSubmissions: cur.ClientSubmissions, ScoreMin: cur.ScoreMin, ScoreMax: cur.ScoreMax,
			ResetSchedule: cur.ResetSchedule, AttemptCap: cur.AttemptCap, Metadata: cur.Metadata,
		})
		if err != nil {
			return err
		}
		w := revisionWrite{
			projectID: projectID, kind: KindLeaderboard, resourceID: id,
			before: before, expected: expected, actor: actor, source: source,
		}
		latest, err := checkRevision(ctx, q, w)
		if err != nil {
			return err
		}
		s, err := value(q)
		if err != nil {
			return err
		}
		s.ScoreOperator = cur.ScoreOperator
		// The collapsed entry model freezes each best under the sort order
		// it was written with, so the direction is only editable while the
		// board has no entries in any period. (A submit racing this check is
		// a milliseconds-wide window; at worst one entry lands under the old
		// order, the same exposure as two adjacent submits.)
		if s.SortOrder != cur.SortOrder {
			hasEntries, err := q.LeaderboardHasEntries(ctx, id)
			if err != nil {
				return err
			}
			if hasEntries {
				return ErrSortOrderLocked
			}
		}
		periodStarted, nextReset := cur.PeriodStartedAt, cur.NextResetAt
		if s.ResetSchedule != cur.ResetSchedule {
			now := time.Now().UTC()
			if next, ok := period.NextReset(s.ResetSchedule, now); ok {
				nextReset = pgtype.Timestamptz{Time: next, Valid: true}
				if !periodStarted.Valid || cur.ResetSchedule == period.ScheduleNone {
					periodStarted = pgtype.Timestamptz{Time: now, Valid: true}
				}
			} else {
				// Schedule turned off: the current period persists, it just
				// stops resetting.
				nextReset = pgtype.Timestamptz{}
			}
		}
		n, err := q.UpdateLeaderboard(ctx, sqlcgen.UpdateLeaderboardParams{
			Name:              s.Name,
			SortOrder:         s.SortOrder,
			Metadata:          s.Metadata,
			ClientSubmissions: s.ClientSubmissions,
			ScoreMin:          s.ScoreMin,
			ScoreMax:          s.ScoreMax,
			ResetSchedule:     s.ResetSchedule,
			AttemptCap:        s.AttemptCap,
			PeriodStartedAt:   periodStarted,
			NextResetAt:       nextReset,
			ProjectID:         projectID,
			ID:                id,
		})
		if err != nil {
			return translateDuplicate(err)
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		if w.after, err = json.Marshal(s); err != nil {
			return err
		}
		if rev, err = recordRevision(ctx, q, latest, w); err != nil {
			return err
		}
		payload := s.auditPayload()
		payload["revision"] = rev
		return actor.audit(ctx, tx, tenantID, projectID, action, strconv.FormatInt(id, 10), payload)
	})
	return rev, err
}

// DeleteLeaderboard soft-deletes a board. Scores stay; RestoreLeaderboard
// undoes it. It returns pgx.ErrNoRows when nothing matched.
func DeleteLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).SoftDeleteLeaderboard(ctx, sqlcgen.SoftDeleteLeaderboardParams{ProjectID: projectID, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return actor.audit(ctx, tx, tenantID, projectID, "leaderboard.delete", strconv.FormatInt(id, 10), nil)
	})
}

// RestoreLeaderboard undoes a soft delete. It returns ErrDuplicateLeaderboard
// when a live board in the project has the same name, and pgx.ErrNoRows when
// no deleted board matched.
func RestoreLeaderboard(ctx context.Context, pool *db.Pool, tenantID, projectID, id int64, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).RestoreLeaderboard(ctx, sqlcgen.RestoreLeaderboardParams{ProjectID: projectID, ID: id})
		if err != nil {
			return translateDuplicate(err)
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return actor.audit(ctx, tx, tenantID, projectID, "leaderboard.restore", strconv.FormatInt(id, 10), nil)
	})
}

// DeletedLeaderboard is a soft-deleted board that can be restored.
type DeletedLeaderboard struct {
	ID        int64
	Name      string
	DeletedAt time.Time
}

// ListDeletedLeaderboards returns the newest 50 deleted boards of a project.
func ListDeletedLeaderboards(ctx context.Context, pool *db.Pool, tenantID, projectID int64) ([]DeletedLeaderboard, error) {
	var out []DeletedLeaderboard
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListDeletedLeaderboardsForProject(ctx, projectID)
		if err != nil {
			return err
		}
		out = make([]DeletedLeaderboard, 0, len(rows))
		for _, r := range rows {
			out = append(out, DeletedLeaderboard{ID: r.ID, Name: r.Name, DeletedAt: r.DeletedAt.Time})
		}
		return nil
	})
	return out, err
}

func translateDuplicate(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicateLeaderboard
	}
	return err
}
