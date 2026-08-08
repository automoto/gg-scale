package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/period"
)

// LeaderboardPeriodResetKind is the River job kind for the scheduled
// leaderboard period reset sweep.
const LeaderboardPeriodResetKind = "leaderboard_period_reset"

// LeaderboardPeriodResetArgs is the (argument-less) periodic reset job.
// River schedules it on the elected leader, so it runs once across the fleet.
type LeaderboardPeriodResetArgs struct{}

// Kind implements river.JobArgs.
func (LeaderboardPeriodResetArgs) Kind() string { return LeaderboardPeriodResetKind }

// LeaderboardPeriodResetWorker archives due leaderboard periods and starts
// the next one.
type LeaderboardPeriodResetWorker struct {
	river.WorkerDefaults[LeaderboardPeriodResetArgs]
	pool *db.Pool
}

// NewLeaderboardPeriodResetWorker returns a worker bound to the app pool.
func NewLeaderboardPeriodResetWorker(pool *db.Pool) *LeaderboardPeriodResetWorker {
	return &LeaderboardPeriodResetWorker{pool: pool}
}

// Work implements river.Worker.
func (w *LeaderboardPeriodResetWorker) Work(ctx context.Context, _ *river.Job[LeaderboardPeriodResetArgs]) error {
	return ResetDueLeaderboardPeriods(ctx, w.pool, time.Now())
}

// ResetDueLeaderboardPeriods advances every board whose reset boundary has
// passed: it archives the finished period (leaderboard_periods row spanning
// period_started_at → the boundary), bumps current_period, and schedules the
// next boundary from now. Entries are never touched — they stay readable
// under their period number. The new period starts AT the passed boundary,
// so periods stay calendar-aligned even when the job runs late; if several
// boundaries were missed (server down), the skipped ones collapse into the
// new period rather than minting empty archive rows.
//
// Tenants are listed via BootstrapQ (top-level scope, no RLS context); each
// tenant's boards are read FOR UPDATE inside its own RLS-scoped transaction,
// so two overlapping runs can never double-archive a period. A single
// cross-tenant due-boards query is not an option: leaderboards FORCEs row
// level security and the app role has no BYPASSRLS, so a scan without a
// tenant GUC sees zero rows. One indexed query per tenant per sweep is the
// price of that isolation (the same shape as every other GC job here).
func ResetDueLeaderboardPeriods(ctx context.Context, pool *db.Pool, now time.Time) error {
	var tenantIDs []int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id FROM tenants ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tenantIDs = append(tenantIDs, id)
		}
		return rows.Err()
	}); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}

	var resets, failed int64
	for _, tenantID := range tenantIDs {
		tctx := db.WithTenant(ctx, tenantID)
		var n int64
		if err := pool.Q(tctx, func(tx pgx.Tx) error {
			var qerr error
			n, qerr = resetDueBoardsForTenant(tctx, sqlcgen.New(tx), now)
			return qerr
		}); err != nil {
			slog.WarnContext(ctx, "leaderboard period reset: tenant failed", "err", err, "tenant_id", tenantID)
			failed++
			continue
		}
		resets += n
	}
	if resets > 0 {
		slog.InfoContext(ctx, "leaderboard period reset", "boards_reset", resets)
	}
	if failed > 0 {
		return fmt.Errorf("leaderboard period reset: %d tenant(s) failed", failed)
	}
	return nil
}

func resetDueBoardsForTenant(ctx context.Context, q *sqlcgen.Queries, now time.Time) (int64, error) {
	due, err := q.ListDueLeaderboardResets(ctx, pgtype.Timestamptz{Time: now, Valid: true})
	if err != nil {
		return 0, err
	}
	var resets int64
	for _, board := range due {
		next, ok := period.NextReset(board.ResetSchedule, now)
		if !ok {
			// The query filters schedule <> 'none'; an unknown schedule can
			// only mean schema drift, so skip rather than corrupt state.
			slog.WarnContext(ctx, "leaderboard period reset: unknown schedule",
				"leaderboard_id", board.ID, "schedule", board.ResetSchedule)
			continue
		}
		startedAt := board.PeriodStartedAt
		if !startedAt.Valid {
			startedAt = board.CreatedAt
		}
		if err := q.ArchiveLeaderboardPeriod(ctx, sqlcgen.ArchiveLeaderboardPeriodParams{
			LeaderboardID: board.ID,
			Period:        board.CurrentPeriod,
			StartedAt:     startedAt,
			EndedAt:       board.NextResetAt,
		}); err != nil {
			return resets, err
		}
		n, err := q.AdvanceLeaderboardPeriod(ctx, sqlcgen.AdvanceLeaderboardPeriodParams{
			// The new period begins at the boundary that just passed, not at
			// the job's run time, so periods stay calendar-aligned.
			PeriodStartedAt: board.NextResetAt,
			NextResetAt:     pgtype.Timestamptz{Time: next, Valid: true},
			ID:              board.ID,
		})
		if err != nil {
			return resets, err
		}
		resets += n
	}
	return resets, nil
}
