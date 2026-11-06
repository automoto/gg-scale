package jobs

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// TenantSignupExpiryKind is the River job kind for the expired signup approval
// sweep.
const TenantSignupExpiryKind = "tenant_signup_expiry"

// TenantSignupExpiryArgs is the (argument-less) periodic job.
type TenantSignupExpiryArgs struct{}

// Kind implements river.JobArgs.
func (TenantSignupExpiryArgs) Kind() string { return TenantSignupExpiryKind }

// TenantSignupExpiryWorker deletes approved signups whose code expired, which
// releases the tenant name and the email they hold. Safe to retry.
type TenantSignupExpiryWorker struct {
	river.WorkerDefaults[TenantSignupExpiryArgs]
	pool *db.Pool
}

// NewTenantSignupExpiryWorker returns a worker bound to the app pool.
func NewTenantSignupExpiryWorker(pool *db.Pool) *TenantSignupExpiryWorker {
	return &TenantSignupExpiryWorker{pool: pool}
}

// Work implements river.Worker.
func (w *TenantSignupExpiryWorker) Work(ctx context.Context, _ *river.Job[TenantSignupExpiryArgs]) error {
	return SweepExpiredTenantSignups(ctx, w.pool)
}

// SweepExpiredTenantSignups deletes every approved signup with an expired code.
// The table is platform-global (no tenant, no RLS), so one BootstrapQ pass
// covers everything.
func SweepExpiredTenantSignups(ctx context.Context, pool *db.Pool) error {
	var deleted int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		deleted, qerr = sqlcgen.New(tx).DeleteExpiredTenantSignupApprovals(ctx)
		return qerr
	}); err != nil {
		return err
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "tenant signup expiry", "deleted", deleted)
	}
	return nil
}
