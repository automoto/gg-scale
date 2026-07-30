package controlpanel

// Forgot-password flow for dashboard users. The emailed token is random and
// stored only as a sha-256 hash; single-use with a short TTL. Every request
// gets the same "If an account matches" response, and the variable-cost work
// (lookup, token mint, SMTP delivery) runs off-request so response timing
// cannot reveal whether the account exists.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/jobs"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	// passwordResetSendTimeout bounds the detached lookup+mint+send fallback
	// when no job queue is available.
	passwordResetSendTimeout = 30 * time.Second
	// maxControlPanelPassBytes is bcrypt's input limit; longer passwords make
	// GenerateFromPassword fail, so reject them as a validation error.
	maxControlPanelPassBytes = 72
)

var errResetTokenInvalid = errors.New("control panel: reset token invalid")

func (h *Handler) forgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, ForgotPasswordPage(ForgotPasswordView{}))
}

func (h *Handler) forgotPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := normalizeEmail(r.Form.Get("email"))
	if validControlPanelEmail(email) {
		h.startPasswordResetDelivery(r.Context(), email)
	}
	webutil.Render(r, w, ForgotPasswordPage(ForgotPasswordView{Submitted: true}))
}

// startPasswordResetDelivery hands the variable-cost work (account lookup,
// token mint, SMTP delivery) to the durable job queue, so every valid-looking
// request costs one job insert regardless of whether the account exists
// (anti-enumeration) and a deploy cannot lose an acknowledged request. When
// the queue is unavailable it degrades to a detached in-process send.
func (h *Handler) startPasswordResetDelivery(ctx context.Context, email string) {
	if h.enqueuePasswordReset != nil {
		if err := h.enqueuePasswordReset(ctx, email); err == nil {
			return
		} else { //nolint:revive // fall through to the in-process fallback on insert failure
			slog.ErrorContext(ctx, "control panel password reset enqueue", "err", err)
		}
	}
	dctx := context.WithoutCancel(ctx)
	go func() {
		dctx, cancel := context.WithTimeout(dctx, passwordResetSendTimeout)
		defer cancel()
		if err := jobs.SendPasswordResetEmail(dctx, h.passwordResetDeps(), jobs.PasswordResetSurfaceControlPanel, email); err != nil {
			slog.ErrorContext(dctx, "control panel password reset send", "err", err)
		}
	}()
}

func (h *Handler) passwordResetDeps() jobs.PasswordResetDeps {
	return jobs.PasswordResetDeps{
		Pool:     h.pool,
		Mailer:   h.mailer,
		MailFrom: h.cfg.MailFrom,
		BaseURL:  h.cfg.BaseURL,
	}
}

func (h *Handler) resetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := h.peekResetToken(r.Context(), token); err != nil {
		h.renderResetTokenError(w, r, err)
		return
	}
	webutil.Render(r, w, ResetPasswordPage(ResetPasswordView{Token: token}))
}

func (h *Handler) resetPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	token := r.Form.Get("token")
	password := r.Form.Get("password")
	// Validate and hash BEFORE consuming: a rejected password (or a bcrypt
	// failure) must not burn the single-use link.
	if len(password) < minControlPanelPassLen || len(password) > maxControlPanelPassBytes {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, ResetPasswordPage(ResetPasswordView{
			Token:       token,
			FieldErrors: map[string]string{"password": "Password must be between 12 and 72 characters"},
		}))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		webutil.InternalError(w, "password reset: bcrypt", err)
		return
	}
	if token == "" {
		h.renderResetTokenError(w, r, errResetTokenInvalid)
		return
	}
	// One transaction: consume the token, set the password, revoke sessions,
	// and burn every other outstanding link. A failure rolls all of it back,
	// so the link is never spent without the password actually changing.
	tokenHash := sha256.Sum256([]byte(token))
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		userID, qerr := q.ConsumeControlPanelPasswordReset(r.Context(), tokenHash[:])
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errResetTokenInvalid
		}
		if qerr != nil {
			return fmt.Errorf("consume reset token: %w", qerr)
		}
		if qerr := q.UpdateControlPanelPassword(r.Context(), sqlcgen.UpdateControlPanelPasswordParams{
			ID: userID, PasswordHash: hash,
		}); qerr != nil {
			return qerr
		}
		// A reset proves email ownership but not possession of a live
		// session — every existing session dies with the old password.
		if qerr := q.RevokeAllControlPanelSessionsForUser(r.Context(), userID); qerr != nil {
			return qerr
		}
		if qerr := q.InvalidateControlPanelPasswordResets(r.Context(), userID); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(r.Context(), tx, userID, "control_panel.password_reset", "", nil)
	})
	if err != nil {
		h.renderResetTokenError(w, r, err)
		return
	}
	webutil.Render(r, w, ResetPasswordDonePage())
}

// peekResetToken is the read-only validity probe for the GET form page; the
// POST path consumes atomically inside its transaction.
func (h *Handler) peekResetToken(ctx context.Context, token string) error {
	if token == "" {
		return errResetTokenInvalid
	}
	hash := sha256.Sum256([]byte(token))
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).PeekControlPanelPasswordReset(ctx, hash[:])
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errResetTokenInvalid
	}
	if err != nil {
		return fmt.Errorf("password reset lookup: %w", err)
	}
	return nil
}

func (h *Handler) renderResetTokenError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errResetTokenInvalid) {
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, ResetPasswordInvalidPage())
		return
	}
	webutil.InternalError(w, "password reset", err)
}
