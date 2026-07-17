package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
)

// ConnectionGrantGCKind is the River job kind for expired regional
// connection-cap grants.
const ConnectionGrantGCKind = "connection_grant_gc"

// ConnectionGrantGCArgs is the argument-less periodic GC job.
type ConnectionGrantGCArgs struct{}

// Kind implements river.JobArgs.
func (ConnectionGrantGCArgs) Kind() string { return ConnectionGrantGCKind }

// ConnectionGrantGCWorker removes grants left behind by stopped processes.
type ConnectionGrantGCWorker struct {
	river.WorkerDefaults[ConnectionGrantGCArgs]
	pool *db.Pool
}

// NewConnectionGrantGCWorker returns a worker bound to the app pool.
func NewConnectionGrantGCWorker(pool *db.Pool) *ConnectionGrantGCWorker {
	return &ConnectionGrantGCWorker{pool: pool}
}

// Work implements river.Worker.
func (w *ConnectionGrantGCWorker) Work(ctx context.Context, _ *river.Job[ConnectionGrantGCArgs]) error {
	return SweepExpiredConnectionGrants(ctx, w.pool)
}

// SweepExpiredConnectionGrants deletes expired rows across all tenants and
// regions. Expired grants are already ignored by admission, so this is safe to
// retry and only bounds table growth after a process disappears.
func SweepExpiredConnectionGrants(ctx context.Context, pool *db.Pool) error {
	var deleted int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `
DELETE FROM realtime_connection_grants
WHERE expires_at <= transaction_timestamp()`)
		if err != nil {
			return fmt.Errorf("delete expired connection grants: %w", err)
		}
		deleted = result.RowsAffected()
		return nil
	}); err != nil {
		return err
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "connection grant GC", "deleted", deleted)
	}
	return nil
}
