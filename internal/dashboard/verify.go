package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	verifyPendingCookieName = "ggscale_dashboard_verify"
	verifyPendingTTL        = 30 * time.Minute
	verifyCodeSubject       = "Your ggscale verification code"
)

var (
	errBadVerifyCode       = errors.New("dashboard: bad verify code")
	errVerifyExpired       = errors.New("dashboard: verify code expired")
	errVerifyLocked        = errors.New("dashboard: verify attempts exhausted")
	errVerifyResendTooSoon = errors.New("dashboard: resend too soon")
	errAlreadyVerified     = errors.New("dashboard: account already verified")
	errVerifyAccountLocked = errors.New("dashboard: account locked after too many verification attempts")
)

// VerifyView is the data rendered by the verification code page.
type VerifyView struct {
	Email   string
	Error   string
	Message string
}

// verifyPendingPayload is the user ID stored in the verify-pending cookie.
// We sign it with the dashboard bootstrap signing key so a stolen cookie
// can't grant verification of a different user. ExpiresAt is a server-
// checked Unix-seconds expiry: the cookie's own MaxAge is client-controlled
// and not trusted.
type verifyPendingPayload struct {
	UserID    int64
	Email     string
	ExpiresAt int64
}

func (h *Handler) setVerifyPendingCookie(w http.ResponseWriter, p verifyPendingPayload) {
	if p.ExpiresAt == 0 {
		p.ExpiresAt = h.now().Add(verifyPendingTTL).Unix()
	}
	value := encodeVerifyCookie(p, h.verifyCookieKey())
	http.SetCookie(w, &http.Cookie{
		Name:     verifyPendingCookieName,
		Value:    value,
		Path:     pathDashboard,
		MaxAge:   int(verifyPendingTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearVerifyPendingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     verifyPendingCookieName,
		Value:    "",
		Path:     pathDashboard,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) verifyPendingFromCookie(r *http.Request) (verifyPendingPayload, bool) {
	c, err := r.Cookie(verifyPendingCookieName)
	if err != nil {
		return verifyPendingPayload{}, false
	}
	p, ok := decodeVerifyCookie(c.Value, h.verifyCookieKey())
	if !ok {
		return verifyPendingPayload{}, false
	}
	// MaxAge is advisory (client-controlled); enforce the server-checked
	// expiry baked into the signed payload.
	if p.ExpiresAt > 0 && h.now().Unix() > p.ExpiresAt {
		return verifyPendingPayload{}, false
	}
	return p, true
}

// verifyCookieKey returns the HMAC key for the verify-pending cookie.
// The key is a 32-byte random secret generated at handler construction;
// it lives only in memory and is rotated on every process restart.
func (h *Handler) verifyCookieKey() []byte {
	return h.verifySigningKey
}

func encodeVerifyCookie(p verifyPendingPayload, key []byte) string {
	payload := fmt.Sprintf("%d:%d:%s", p.UserID, p.ExpiresAt, p.Email)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func decodeVerifyCookie(raw string, key []byte) (verifyPendingPayload, bool) {
	encPayload, encSig, ok := strings.Cut(raw, ".")
	if !ok {
		return verifyPendingPayload{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return verifyPendingPayload{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return verifyPendingPayload{}, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
		return verifyPendingPayload{}, false
	}
	// payload layout: userID:expiresAtUnix:email
	idRaw, rest, ok := strings.Cut(string(payload), ":")
	if !ok {
		return verifyPendingPayload{}, false
	}
	expRaw, email, ok := strings.Cut(rest, ":")
	if !ok {
		return verifyPendingPayload{}, false
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		return verifyPendingPayload{}, false
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return verifyPendingPayload{}, false
	}
	return verifyPendingPayload{UserID: id, ExpiresAt: exp, Email: email}, true
}

// startVerification mints a fresh 6-digit code, persists it, and emails
// it. Caller has already validated the user's password.
func (h *Handler) startVerification(ctx context.Context, userID int64, email string) error {
	state, err := h.fetchDashboardVerifyState(ctx, userID)
	if err != nil {
		return err
	}
	if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
		// Lifetime lockout blocks resend too — otherwise an attacker
		// loops fresh codes around the per-code attempts cap forever.
		return errVerifyAccountLocked
	}
	if !verifycode.CanResend(state.EmailVerificationLastSentAt.Time, h.now()) {
		return errVerifyResendTooSoon
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		return fmt.Errorf("verify code: %w", err)
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		return fmt.Errorf("verify salt: %w", err)
	}
	codeHash := verifycode.Hash(salt, code)
	expiresAt := h.now().Add(verifycode.CodeTTL)
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SetDashboardUserVerificationCode(ctx, sqlcgen.SetDashboardUserVerificationCodeParams{
			ID:        userID,
			CodeHash:  codeHash,
			CodeSalt:  salt,
			ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
	})
	if err != nil {
		return err
	}
	if h.mailer != nil && h.cfg.MailFrom != "" {
		_ = h.mailer.Send(ctx, mailer.Message{
			From:    h.cfg.MailFrom,
			To:      []string{email},
			Subject: verifyCodeSubject,
			Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
		})
	}
	return nil
}

func (h *Handler) fetchDashboardVerifyState(ctx context.Context, userID int64) (sqlcgen.GetDashboardUserVerificationStateRow, error) {
	var row sqlcgen.GetDashboardUserVerificationStateRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetDashboardUserVerificationState(ctx, userID)
		return err
	})
	return row, err
}

func (h *Handler) confirmVerification(ctx context.Context, userID int64, code string) error {
	state, err := h.fetchDashboardVerifyState(ctx, userID)
	if err != nil {
		return err
	}
	if state.EmailVerifiedAt.Valid {
		return errAlreadyVerified
	}
	if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
		return errVerifyAccountLocked
	}
	if verifycode.Expired(state.EmailVerificationExpiresAt.Time, h.now()) {
		return errVerifyExpired
	}
	if len(state.EmailVerificationSalt) == 0 || len(state.EmailVerificationCodeHash) == 0 {
		return errVerifyExpired
	}
	// Reserve the attempt atomically. Two parallel wrong codes used to both
	// read attempts < cap and both proceed; this query folds the check into
	// the same UPDATE so only one passes when the cap would otherwise be
	// crossed. Zero rows → already at cap.
	var reserved sqlcgen.ReserveDashboardVerifyAttemptRow
	resErr := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		reserved, qerr = sqlcgen.New(tx).ReserveDashboardVerifyAttempt(ctx, sqlcgen.ReserveDashboardVerifyAttemptParams{
			ID:          userID,
			MaxAttempts: int32(verifycode.MaxAttempts),
		})
		return qerr
	})
	if errors.Is(resErr, pgx.ErrNoRows) {
		return errVerifyLocked
	}
	if resErr != nil {
		return resErr
	}
	// Lifetime cap survives /resend (which only resets the per-code
	// counter). Reaching it locks the account for LockoutDuration so an
	// attacker can't loop resend → 5 attempts → resend → … forever.
	if verifycode.LifetimeExhausted(int(reserved.EmailVerificationLifetimeAttempts)) {
		lockedUntil := pgtype.Timestamptz{Time: h.now().Add(verifycode.LockoutDuration), Valid: true}
		_ = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).LockDashboardUserVerification(ctx, sqlcgen.LockDashboardUserVerificationParams{
				ID: userID, LockedUntil: lockedUntil,
			})
		})
		return errVerifyAccountLocked
	}
	expected := verifycode.Hash(state.EmailVerificationSalt, code)
	if subtle.ConstantTimeCompare(expected, state.EmailVerificationCodeHash) != 1 {
		return errBadVerifyCode
	}
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).MarkDashboardUserVerified(ctx, userID)
	})
}

func (h *Handler) verifyPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.verifyPendingFromCookie(r)
	if !ok {
		http.Redirect(w, r, pathDashboardLogin, http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Message: r.URL.Query().Get("flash")}))
}

func (h *Handler) verifyHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.verifyPendingFromCookie(r)
	if !ok {
		http.Redirect(w, r, pathDashboardLogin, http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.Form.Get("code"))
	if len(code) != 6 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "Enter the 6-digit code from your email."}))
		return
	}
	err := h.confirmVerification(r.Context(), p.UserID, code)
	switch {
	case errors.Is(err, errAlreadyVerified):
		// Account already verified — do not issue a session from the verify
		// path. Send the user to login instead so they re-authenticate with
		// a password.
		h.clearVerifyPendingCookie(w)
		http.Redirect(w, r, "/v1/dashboard/login?flash="+url.QueryEscape("Your email is already verified. Please sign in."), http.StatusSeeOther)
		return
	case errors.Is(err, errBadVerifyCode):
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "That code is incorrect. Try again."}))
		return
	case errors.Is(err, errVerifyExpired):
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "That code has expired. Request a new one."}))
		return
	case errors.Is(err, errVerifyLocked):
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "Too many attempts. Request a new code."}))
		return
	case errors.Is(err, errVerifyAccountLocked):
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "This account is locked after too many verification attempts. Contact support to unlock."}))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "dashboard verify confirm", "err", err)
		http.Error(w, "verification error", http.StatusInternalServerError)
		return
	}

	// Verified — issue a session and clear the verify-pending cookie.
	h.clearVerifyPendingCookie(w)
	if _, err := h.issueSession(r.Context(), w, p.UserID, h.clientIP(r), r.UserAgent()); err != nil {
		slog.ErrorContext(r.Context(), "dashboard verify session", "err", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, pathDashboard, http.StatusSeeOther)
}

func (h *Handler) verifyResendHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.verifyPendingFromCookie(r)
	if !ok {
		http.Redirect(w, r, pathDashboardLogin, http.StatusSeeOther)
		return
	}
	err := h.startVerification(r.Context(), p.UserID, p.Email)
	switch {
	case errors.Is(err, errVerifyResendTooSoon):
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "Wait a minute between resends."}))
		return
	case errors.Is(err, errVerifyAccountLocked):
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(VerifyView{Email: p.Email, Error: "This account is locked after too many verification attempts. Contact support to unlock."}))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "dashboard verify resend", "err", err)
		http.Error(w, "resend error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/v1/dashboard/verify?flash="+url.QueryEscape("A new code was sent."), http.StatusSeeOther)
}
