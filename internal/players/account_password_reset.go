package players

// Forgot-password flow for global player accounts. Mirrors the control-panel
// flow: random token emailed, sha-256 hash stored, single-use, short TTL, a
// constant "If an account matches" response, and off-request delivery so
// response timing cannot reveal whether the account exists.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/jobs"
	"github.com/automoto/gg-scale/internal/webutil"
)

// accountResetSendTimeout bounds the detached lookup+mint+send fallback when
// no job queue is available.
const accountResetSendTimeout = 30 * time.Second

var errAccountResetTokenInvalid = errors.New("players: reset token invalid")

func (h *Handler) accountForgotPasswordPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, AccountForgotPasswordPage(AccountForgotPasswordView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountForgotPassword(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	if validEmail(email) {
		h.startAccountPasswordResetDelivery(r.Context(), email)
	}
	webutil.Render(r, w, AccountForgotPasswordPage(AccountForgotPasswordView{
		CSRFToken: h.csrf(r),
		Submitted: true,
	}))
}

// startAccountPasswordResetDelivery hands the variable-cost work (account
// lookup, token mint, SMTP delivery) to the durable job queue, so every
// valid-looking request costs one job insert regardless of whether the
// account exists (anti-enumeration) and a deploy cannot lose an acknowledged
// request. When the queue is unavailable it degrades to a detached
// in-process send.
func (h *Handler) startAccountPasswordResetDelivery(ctx context.Context, email string) {
	if h.enqueuePasswordReset != nil {
		if err := h.enqueuePasswordReset(ctx, email); err == nil {
			return
		} else { //nolint:revive // fall through to the in-process fallback on insert failure
			slog.ErrorContext(ctx, "player account password reset enqueue", "err", err)
		}
	}
	dctx := context.WithoutCancel(ctx)
	go func() {
		dctx, cancel := context.WithTimeout(dctx, accountResetSendTimeout)
		defer cancel()
		deps := jobs.PasswordResetDeps{Pool: h.pool, Mailer: h.mailer, MailFrom: h.mailFrom, BaseURL: h.cfg.BaseURL}
		if err := jobs.SendPasswordResetEmail(dctx, deps, jobs.PasswordResetSurfacePlayerAccount, email); err != nil {
			slog.ErrorContext(dctx, "player account password reset send", "err", err)
		}
	}()
}

func (h *Handler) accountResetPasswordPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := h.peekAccountResetToken(r.Context(), token); err != nil {
		h.renderAccountResetTokenError(w, r, err)
		return
	}
	webutil.Render(r, w, AccountResetPasswordPage(AccountResetPasswordView{
		CSRFToken: h.csrf(r),
		Token:     token,
	}))
}

func (h *Handler) accountResetPassword(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	token := r.Form.Get("token")
	password := r.Form.Get("password")
	// Validate and hash BEFORE consuming: a rejected password (or a bcrypt
	// failure) must not burn the single-use link.
	if !validPlayerPassword(password) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountResetPasswordPage(AccountResetPasswordView{
			CSRFToken:   h.csrf(r),
			Token:       token,
			FieldErrors: map[string]string{"password": "Password must be between 8 and 72 characters."},
		}))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		webutil.InternalError(w, "account password reset: bcrypt", err)
		return
	}
	if token == "" {
		h.renderAccountResetTokenError(w, r, errAccountResetTokenInvalid)
		return
	}
	// One transaction: consume the token, set the password (epoch bump kills
	// live sessions), revoke the refresh rows, and burn every other
	// outstanding link. A failure rolls all of it back, so the link is never
	// spent without the password actually changing.
	tokenHash := sha256.Sum256([]byte(token))
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		accountID, qerr := q.ConsumePlayerAccountPasswordReset(r.Context(), tokenHash[:])
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errAccountResetTokenInvalid
		}
		if qerr != nil {
			return fmt.Errorf("consume reset token: %w", qerr)
		}
		if qerr := q.SetPlayerAccountPassword(r.Context(), sqlcgen.SetPlayerAccountPasswordParams{
			ID: accountID, PasswordHash: hash,
		}); qerr != nil {
			return qerr
		}
		if qerr := q.RevokeAllPlayerAccountSessions(r.Context(), accountID); qerr != nil {
			return qerr
		}
		// A password change is exactly when remembered devices should stop
		// skipping the 2FA challenge — a stale trusted-device cookie must not
		// survive a reset triggered by a compromised inbox. This is the only
		// change-password surface player accounts have.
		if qerr := q.DeletePlayerAccountTrustedDevicesForAccount(r.Context(), accountID); qerr != nil {
			return qerr
		}
		return q.InvalidatePlayerAccountPasswordResets(r.Context(), accountID)
	})
	if err != nil {
		h.renderAccountResetTokenError(w, r, err)
		return
	}
	webutil.Render(r, w, AccountResetPasswordDonePage())
}

// peekAccountResetToken is the read-only validity probe for the GET form
// page; the POST path consumes atomically inside its transaction.
func (h *Handler) peekAccountResetToken(ctx context.Context, token string) error {
	if token == "" {
		return errAccountResetTokenInvalid
	}
	hash := sha256.Sum256([]byte(token))
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).PeekPlayerAccountPasswordReset(ctx, hash[:])
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errAccountResetTokenInvalid
	}
	if err != nil {
		return fmt.Errorf("account password reset lookup: %w", err)
	}
	return nil
}

func (h *Handler) renderAccountResetTokenError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errAccountResetTokenInvalid) {
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, AccountResetPasswordInvalidPage())
		return
	}
	webutil.InternalError(w, "account password reset", err)
}
