package jobs

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/webutil"
)

// PasswordResetEmailKind is the River job kind for durable forgot-password
// delivery. The HTTP handler inserts one job per valid-looking request —
// constant work whether or not the account exists — and the worker does the
// account lookup, token mint, and SMTP send. Durability means a deploy
// between "success page rendered" and "email sent" cannot lose the link.
const PasswordResetEmailKind = "password_reset_email"

// Password-reset surfaces.
const (
	PasswordResetSurfaceControlPanel  = "control_panel"
	PasswordResetSurfacePlayerAccount = "player_account"
)

// PasswordResetEmailArgs identifies which auth stack the address belongs to.
type PasswordResetEmailArgs struct {
	Surface string `json:"surface"`
	Email   string `json:"email"`
}

// Kind implements river.JobArgs.
func (PasswordResetEmailArgs) Kind() string { return PasswordResetEmailKind }

// PasswordResetEmailWorker delivers forgot-password emails for both stacks.
type PasswordResetEmailWorker struct {
	river.WorkerDefaults[PasswordResetEmailArgs]
	deps PasswordResetDeps
}

// PasswordResetDeps carries what the delivery flow needs. BaseURL is the
// externally-visible origin for the emailed links (empty renders a relative
// path, as elsewhere).
type PasswordResetDeps struct {
	Pool     *db.Pool
	Mailer   mailer.Mailer
	MailFrom string
	BaseURL  string
}

// NewPasswordResetEmailWorker returns the delivery worker.
func NewPasswordResetEmailWorker(deps PasswordResetDeps) *PasswordResetEmailWorker {
	return &PasswordResetEmailWorker{deps: deps}
}

// Work implements river.Worker.
func (w *PasswordResetEmailWorker) Work(ctx context.Context, job *river.Job[PasswordResetEmailArgs]) error {
	return SendPasswordResetEmail(ctx, w.deps, job.Args.Surface, job.Args.Email)
}

// SendPasswordResetEmail runs the full flow for one request: look the address
// up, and when it belongs to an enabled account, mint a hashed single-use
// token and email the link. Unknown or disabled addresses are a silent no-op.
// Also called directly (in a detached goroutine) when the job queue is
// unavailable, so self-host without River keeps a working forgot-password.
func SendPasswordResetEmail(ctx context.Context, d PasswordResetDeps, surface, email string) error {
	if d.Mailer == nil || d.MailFrom == "" {
		return nil
	}
	switch surface {
	case PasswordResetSurfaceControlPanel:
		return sendControlPanelPasswordReset(ctx, d, email)
	case PasswordResetSurfacePlayerAccount:
		return sendPlayerAccountPasswordReset(ctx, d, email)
	default:
		// A bad surface is a programming error; retrying cannot fix it.
		slog.ErrorContext(ctx, "password reset: unknown surface", "surface", surface)
		return nil
	}
}

func sendControlPanelPasswordReset(ctx context.Context, d PasswordResetDeps, email string) error {
	var row sqlcgen.GetControlPanelUserAnyStatusByEmailRow
	err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetControlPanelUserAnyStatusByEmail(ctx, email)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.DisabledAt.Valid {
		return nil
	}

	token, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	if err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).CreateControlPanelPasswordReset(ctx, sqlcgen.CreateControlPanelPasswordResetParams{
			ControlPanelUserID: row.ID,
			TokenHash:          hash[:],
			ExpiresAt:          pgtype.Timestamptz{Time: time.Now().Add(webutil.PasswordResetTTL), Valid: true},
		})
	}); err != nil {
		return err
	}

	link := strings.TrimRight(d.BaseURL, "/") + "/v1/control-panel/reset-password?token=" + token
	return d.Mailer.Send(ctx, mailer.Message{
		From:    d.MailFrom,
		To:      []string{email},
		Subject: "Reset your ggscale password",
		Body: "A password reset was requested for your ggscale control panel account.\n\n" +
			"Set a new password here: " + link + "\n\n" +
			"The link works once and expires in 1 hour. If you did not request this, ignore this email.",
	})
}

func sendPlayerAccountPasswordReset(ctx context.Context, d PasswordResetDeps, email string) error {
	var row sqlcgen.GetPlayerAccountByEmailRow
	err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(ctx, email)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.DisabledAt.Valid {
		return nil
	}

	token, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	if err := d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).CreatePlayerAccountPasswordReset(ctx, sqlcgen.CreatePlayerAccountPasswordResetParams{
			PlayerAccountID: row.ID,
			TokenHash:       hash[:],
			ExpiresAt:       pgtype.Timestamptz{Time: time.Now().Add(webutil.PasswordResetTTL), Valid: true},
		})
	}); err != nil {
		return err
	}

	link := strings.TrimRight(d.BaseURL, "/") + "/v1/players/account/reset-password?token=" + token
	return d.Mailer.Send(ctx, mailer.Message{
		From:    d.MailFrom,
		To:      []string{email},
		Subject: "Reset your gg-scale account password",
		Body: "A password reset was requested for your gg-scale account.\n\n" +
			"Set a new password here: " + link + "\n\n" +
			"The link works once and expires in 1 hour. If you did not request this, ignore this email.",
	})
}

// PasswordResetGCKind is the River job kind for the reset-token retention
// sweep.
const PasswordResetGCKind = "password_reset_gc"

// PasswordResetGCArgs is the (argument-less) periodic GC job.
type PasswordResetGCArgs struct{}

// Kind implements river.JobArgs.
func (PasswordResetGCArgs) Kind() string { return PasswordResetGCKind }

// PasswordResetGCWorker deletes reset tokens a day past expiry on both auth
// stacks. Expired rows are already inert — every lookup filters expires_at —
// so the sweep is pure hygiene and safe to retry.
type PasswordResetGCWorker struct {
	river.WorkerDefaults[PasswordResetGCArgs]
	pool *db.Pool
}

// NewPasswordResetGCWorker returns a worker bound to the app pool.
func NewPasswordResetGCWorker(pool *db.Pool) *PasswordResetGCWorker {
	return &PasswordResetGCWorker{pool: pool}
}

// Work implements river.Worker.
func (w *PasswordResetGCWorker) Work(ctx context.Context, _ *river.Job[PasswordResetGCArgs]) error {
	return SweepExpiredPasswordResets(ctx, w.pool)
}

// SweepExpiredPasswordResets removes long-expired reset tokens. The tables
// are platform-global (no tenant, no RLS), so one BootstrapQ pass covers both.
func SweepExpiredPasswordResets(ctx context.Context, pool *db.Pool) error {
	var controlPanel, players int64
	if err := pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		if controlPanel, qerr = q.DeleteExpiredControlPanelPasswordResets(ctx); qerr != nil {
			return qerr
		}
		players, qerr = q.DeleteExpiredPlayerAccountPasswordResets(ctx)
		return qerr
	}); err != nil {
		return err
	}
	if controlPanel > 0 || players > 0 {
		slog.InfoContext(ctx, "password reset GC", "control_panel_deleted", controlPanel, "players_deleted", players)
	}
	return nil
}
