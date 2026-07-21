// Package relaymeter meters managed-relay credential issuances against the
// class RelaySessionsPerMonth allowance. It
// follows the platform quota policy: warn the tenant's admins at 80% and
// 100%, then refuse only NEW credential issuance — in-flight TURN sessions
// are never dropped, and the counter resets each calendar month. Tenants
// without enforce_quotas are counted but never refused (zero-config
// self-host stays unmetered).
package relaymeter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/tenant"
)

// Meter counts relay credential issuances per tenant per calendar month and
// enforces the class allowance for quota-enforced tenants.
type Meter struct {
	pool     *db.Pool
	mailer   mailer.Mailer
	mailFrom string
	now      func() time.Time
}

// New builds a Meter. mailer may be nil (warnings are then log-only).
func New(pool *db.Pool, m mailer.Mailer, mailFrom string) *Meter {
	return &Meter{pool: pool, mailer: m, mailFrom: mailFrom, now: time.Now}
}

// Allow counts one credential issuance for the tenant and reports whether it
// may proceed. Past the allowance it returns *quota.ErrQuotaExceeded (the
// refused attempt is not counted). The count, the allowance check, and the
// warn-claim updates run in one tenant-context transaction; warning emails go
// out after commit, best-effort.
func (m *Meter) Allow(ctx context.Context, tenantID int64) error {
	month := monthStart(m.now())
	var warn80, warn100 bool
	var allowance, sessions int64

	tctx := db.WithTenant(ctx, tenantID)
	err := m.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		qc, err := q.GetTenantQuotaContext(tctx)
		if err != nil {
			return fmt.Errorf("relay meter: quota context: %w", err)
		}
		allowance = quota.Unlimited
		if qc.EnforceQuotas {
			allowance = quota.LimitsForClass(tenant.ClampTier(int(qc.Tier))).RelaySessionsPerMonth
		}

		sessions, err = q.IncrementRelaySessions(tctx, sqlcgen.IncrementRelaySessionsParams{
			Month:     pgtype.Date{Time: month, Valid: true},
			Allowance: allowance,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return &quota.ErrQuotaExceeded{Axis: quota.AxisRelaySessions, Limit: allowance, Current: allowance}
		}
		if err != nil {
			return fmt.Errorf("relay meter: increment: %w", err)
		}
		if allowance == quota.Unlimited {
			return nil
		}

		monthArg := pgtype.Date{Time: month, Valid: true}
		if crossed80(sessions, allowance) {
			n, err := q.MarkRelayUsageWarned80(tctx, monthArg)
			if err != nil {
				return fmt.Errorf("relay meter: mark 80%%: %w", err)
			}
			warn80 = n > 0
		}
		if crossed100(sessions, allowance) {
			n, err := q.MarkRelayUsageWarned100(tctx, monthArg)
			if err != nil {
				return fmt.Errorf("relay meter: mark 100%%: %w", err)
			}
			warn100 = n > 0
		}
		return nil
	})
	if err != nil {
		return err
	}

	if warn80 {
		m.sendWarning(ctx, tenantID, warn80Email(sessions, allowance))
	}
	if warn100 {
		m.sendWarning(ctx, tenantID, warn100Email(allowance))
	}
	return nil
}

// monthStart is the UTC first-of-month key for the usage row.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func crossed80(sessions, allowance int64) bool {
	return sessions*100 >= allowance*80
}

func crossed100(sessions, allowance int64) bool {
	return sessions >= allowance
}

type warningEmail struct {
	subject, body string
}

func warn80Email(sessions, allowance int64) warningEmail {
	return warningEmail{
		subject: "Your ggscale relay usage is at 80% of this month's allowance",
		body: fmt.Sprintf("Your tenant's managed relay has used %d of its %d relay sessions for this month.\n\n"+
			"Once the allowance is exhausted, new relay sessions are refused until it resets on the 1st; "+
			"sessions already connected are never dropped. Request a tier upgrade from tenant settings to raise the allowance.",
			sessions, allowance),
	}
}

func warn100Email(allowance int64) warningEmail {
	return warningEmail{
		subject: "Your ggscale relay allowance for this month is used up",
		body: fmt.Sprintf("Your tenant's managed relay has reached its monthly allowance of %d relay sessions.\n\n"+
			"New relay sessions are refused until the allowance resets on the 1st; sessions already connected "+
			"are never dropped. Request a tier upgrade from tenant settings to raise the allowance.",
			allowance),
	}
}

// sendWarning emails the tenant's owner/admins, mirroring the storage-quota
// notice pattern. Failures are logged, never surfaced to the caller: the
// credential issuance itself already succeeded.
func (m *Meter) sendWarning(ctx context.Context, tenantID int64, email warningEmail) {
	if m.mailer == nil || m.mailFrom == "" {
		slog.WarnContext(ctx, "relay meter: allowance warning (no mailer configured)",
			"tenant_id", tenantID, "subject", email.subject)
		return
	}
	var emails []string
	if err := m.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		emails, e = sqlcgen.New(tx).ListTenantAdminEmails(ctx, tenantID)
		return e
	}); err != nil {
		slog.ErrorContext(ctx, "relay meter: list admin emails", "err", err, "tenant_id", tenantID)
		return
	}
	if len(emails) == 0 {
		return
	}
	if err := m.mailer.Send(ctx, mailer.Message{
		From: m.mailFrom, To: emails, Subject: email.subject, Body: email.body,
	}); err != nil {
		slog.ErrorContext(ctx, "relay meter: warning mailer", "err", err, "tenant_id", tenantID)
	}
}
