package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/tenant"
)

// StorageWarnKind is the River job kind for the storage-quota warning sweep.
const StorageWarnKind = "storage_warn"

// StorageWarnArgs is the (argument-less) periodic job. River schedules it on
// the elected leader, so tenants are notified once across the fleet.
type StorageWarnArgs struct{}

// Kind implements river.JobArgs.
func (StorageWarnArgs) Kind() string { return StorageWarnKind }

// StorageWarnWorker scans enforced tenants' metered storage and emails tenant
// admins when usage crosses 80% or 100% of the class limit. A per-tenant
// last-notified-threshold record prevents re-mailing every run.
type StorageWarnWorker struct {
	river.WorkerDefaults[StorageWarnArgs]
	pool     *db.Pool
	mailer   mailer.Mailer
	mailFrom string
}

// NewStorageWarnWorker binds the worker to the app pool and mailer. A nil
// mailer leaves upward thresholds unrecorded so a later sweep can retry after
// delivery is configured.
func NewStorageWarnWorker(pool *db.Pool, m mailer.Mailer, mailFrom string) *StorageWarnWorker {
	return &StorageWarnWorker{pool: pool, mailer: m, mailFrom: mailFrom}
}

// Work implements river.Worker.
func (w *StorageWarnWorker) Work(ctx context.Context, _ *river.Job[StorageWarnArgs]) error {
	return w.sweep(ctx)
}

// sweep evaluates every enforced tenant's usage against its class limit and
// notifies on an upward threshold crossing. tenant_storage_usage is a
// platform-global table (no RLS), so it reads and updates via BootstrapQ.
func (w *StorageWarnWorker) sweep(ctx context.Context) error {
	var rows []sqlcgen.ListEnforcedTenantStorageRow
	if err := w.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListEnforcedTenantStorage(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("storage warn: list enforced tenants: %w", err)
	}

	for _, r := range rows {
		limit := quota.LimitsForClass(tenant.ClampTier(int(r.Tier))).StorageBytes
		want := storageThreshold(r.TotalBytes, limit)
		if want == r.LastNotifiedThreshold {
			continue
		}
		// Email only on an upward crossing. Record it only after delivery so a
		// transient mail failure is retried by the next sweep. A drop resets the
		// record immediately so a later re-crossing can notify again.
		if want > r.LastNotifiedThreshold {
			if err := w.notify(ctx, r.TenantID, r.Name, r.TotalBytes, limit, want); err != nil {
				slog.ErrorContext(ctx, "storage warn: notify", "err", err, "tenant_id", r.TenantID)
				continue
			}
		}
		if err := w.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SetTenantStorageNotifiedThreshold(ctx, sqlcgen.SetTenantStorageNotifiedThresholdParams{
				Threshold: want,
				TenantID:  r.TenantID,
			})
		}); err != nil {
			slog.ErrorContext(ctx, "storage warn: update threshold", "err", err, "tenant_id", r.TenantID)
			continue
		}
	}
	return nil
}

// storageThreshold returns the highest crossed warning threshold (0, 80, or
// 100) for total bytes against limit. Unlimited/unknown limits never warn.
func storageThreshold(total, limit int64) int16 {
	switch {
	case limit <= 0:
		return 0
	case total >= limit:
		return 100
	case total*100 >= limit*80:
		return 80
	default:
		return 0
	}
}

func (w *StorageWarnWorker) notify(ctx context.Context, tenantID int64, name string, total, limit int64, threshold int16) error {
	if w.mailer == nil {
		return errors.New("mailer is not configured")
	}
	var emails []string
	if err := w.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		emails, err = sqlcgen.New(tx).ListTenantAdminEmails(ctx, tenantID)
		return err
	}); err != nil {
		return fmt.Errorf("list admin emails: %w", err)
	}
	if len(emails) == 0 {
		return errors.New("tenant has no verified admin email recipients")
	}
	subject, body := storageWarnEmail(name, total, limit, threshold)
	if err := w.mailer.Send(ctx, mailer.Message{From: w.mailFrom, To: emails, Subject: subject, Body: body}); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

func storageWarnEmail(name string, total, limit int64, threshold int16) (subject, body string) {
	if threshold >= 100 {
		subject = fmt.Sprintf("Storage limit reached for %s", name)
		body = fmt.Sprintf("Your ggscale tenant %q has reached its storage limit "+
			"(%s of %s used). New object writes that would grow storage are now blocked; "+
			"reads and deletes continue to work. Free space or request a tier upgrade from tenant settings.",
			name, humanizeBytes(total), humanizeBytes(limit))
		return subject, body
	}
	subject = fmt.Sprintf("Storage at 80%% for %s", name)
	body = fmt.Sprintf("Your ggscale tenant %q is using %s of its %s storage limit (over 80%%). "+
		"Free space or request a tier upgrade from tenant settings before writes are blocked at 100%%.",
		name, humanizeBytes(total), humanizeBytes(limit))
	return subject, body
}

// humanizeBytes renders a byte count as GB/MB/KB with one decimal, for emails
// and the settings page.
func humanizeBytes(b int64) string {
	const (
		kb = int64(1) << 10
		mb = int64(1) << 20
		gb = int64(1) << 30
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
