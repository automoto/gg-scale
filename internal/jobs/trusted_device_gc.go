package jobs

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// TrustedDeviceGCKind is the River job kind for the expired trusted-device
// sweep (the "remember this device for 30 days" 2FA grants).
const TrustedDeviceGCKind = "trusted_device_gc"

// TrustedDeviceGCArgs is the (argument-less) periodic GC job.
type TrustedDeviceGCArgs struct{}

// Kind implements river.JobArgs.
func (TrustedDeviceGCArgs) Kind() string { return TrustedDeviceGCKind }

// TrustedDeviceGCWorker deletes expired control-panel and player-account
// trusted-device rows. Expired rows are already inert — validation checks
// expires_at — so this sweep is pure hygiene and safe to retry.
type TrustedDeviceGCWorker struct {
	river.WorkerDefaults[TrustedDeviceGCArgs]
	pool *db.Pool
}

// NewTrustedDeviceGCWorker returns a worker bound to the app pool.
func NewTrustedDeviceGCWorker(pool *db.Pool) *TrustedDeviceGCWorker {
	return &TrustedDeviceGCWorker{pool: pool}
}

// Work implements river.Worker.
func (w *TrustedDeviceGCWorker) Work(ctx context.Context, _ *river.Job[TrustedDeviceGCArgs]) error {
	return SweepExpiredTrustedDevices(ctx, w.pool)
}

// SweepExpiredTrustedDevices removes expired trusted-device rows on both
// login surfaces. The tables are platform-global (no tenant, no RLS), so a
// single BootstrapQ pass covers everything.
func SweepExpiredTrustedDevices(ctx context.Context, pool *db.Pool) error {
	var controlPanel, players int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		if controlPanel, qerr = q.DeleteExpiredControlPanelTrustedDevices(ctx); qerr != nil {
			return qerr
		}
		players, qerr = q.DeleteExpiredPlayerAccountTrustedDevices(ctx)
		return qerr
	}); err != nil {
		return err
	}
	if controlPanel > 0 || players > 0 {
		slog.InfoContext(ctx, "trusted device GC", "control_panel_deleted", controlPanel, "players_deleted", players)
	}
	return nil
}
