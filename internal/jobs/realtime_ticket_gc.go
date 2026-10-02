package jobs

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// RealtimeTicketGCKind is the River job kind for the expired realtime ticket
// sweep.
const RealtimeTicketGCKind = "realtime_ticket_gc"

// RealtimeTicketGCArgs is the (argument-less) periodic GC job.
type RealtimeTicketGCArgs struct{}

// Kind implements river.JobArgs.
func (RealtimeTicketGCArgs) Kind() string { return RealtimeTicketGCKind }

// RealtimeTicketGCWorker deletes realtime tickets that expired without use.
// A used ticket is deleted when it is used, and an expired one cannot be
// used, so this sweep only keeps the table small.
type RealtimeTicketGCWorker struct {
	river.WorkerDefaults[RealtimeTicketGCArgs]
	pool *db.Pool
}

// NewRealtimeTicketGCWorker returns a worker bound to the app pool.
func NewRealtimeTicketGCWorker(pool *db.Pool) *RealtimeTicketGCWorker {
	return &RealtimeTicketGCWorker{pool: pool}
}

// Work implements river.Worker.
func (w *RealtimeTicketGCWorker) Work(ctx context.Context, _ *river.Job[RealtimeTicketGCArgs]) error {
	var deleted int64
	if err := w.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		deleted, err = sqlcgen.New(tx).DeleteExpiredRealtimeTickets(ctx)
		return err
	}); err != nil {
		return err
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "realtime ticket GC", "deleted", deleted)
	}
	return nil
}
