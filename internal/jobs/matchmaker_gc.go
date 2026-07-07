package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// MatchmakerGCKind is the River job kind for the matchmaker retention sweep.
const MatchmakerGCKind = "matchmaker_gc"

// matchmakerTicketRetention is how long terminal (matched/cancelled/failed)
// tickets are kept for polling and debugging before the sweep drops them.
const matchmakerTicketRetention = 24 * time.Hour

// MatchmakerGCArgs is the (argument-less) periodic GC job. River schedules it
// on the elected leader, so it runs once across the fleet rather than once
// per instance.
type MatchmakerGCArgs struct{}

// Kind implements river.JobArgs.
func (MatchmakerGCArgs) Kind() string { return MatchmakerGCKind }

// MatchmakerGCWorker deletes expired matchmaker_matches rows and terminal
// tickets past retention.
type MatchmakerGCWorker struct {
	river.WorkerDefaults[MatchmakerGCArgs]
	pool *db.Pool
}

// NewMatchmakerGCWorker returns a worker bound to the app pool.
func NewMatchmakerGCWorker(pool *db.Pool) *MatchmakerGCWorker {
	return &MatchmakerGCWorker{pool: pool}
}

// Work implements river.Worker by sweeping matchmaker retention.
func (w *MatchmakerGCWorker) Work(ctx context.Context, _ *river.Job[MatchmakerGCArgs]) error {
	return SweepMatchmakerRecords(ctx, w.pool)
}

// SweepMatchmakerRecords drops match rows past their retention window and
// terminal tickets older than matchmakerTicketRetention. Runs GUC-less under
// the matchmaker worker RLS policies; deletes are idempotent, so River's
// retry-with-backoff covers transient failures.
func SweepMatchmakerRecords(ctx context.Context, pool *db.Pool) error {
	return pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		matches, err := q.DeleteExpiredMatchmakerMatches(ctx)
		if err != nil {
			return err
		}
		tickets, err := q.DeleteTerminalMatchmakerTickets(ctx, pgtype.Interval{
			Microseconds: matchmakerTicketRetention.Microseconds(),
			Valid:        true,
		})
		if err != nil {
			return err
		}
		if matches > 0 || tickets > 0 {
			slog.InfoContext(ctx, "matchmaker GC", "matches_deleted", matches, "tickets_deleted", tickets)
		}
		return nil
	})
}
