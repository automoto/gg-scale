package controlpanel

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	twoFactorPendingCookieName = "ggscale_control_panel_2fa"
	trustedDeviceCookieName    = "ggscale_control_panel_trust"
	pathControlPanelLogin2FA   = "/v1/control-panel/login/2fa"
	pathControlPanelAccount    = "/v1/control-panel/account/password"
	// twoFactorIssuer is distinct from the player-site issuer so someone
	// enrolled on both surfaces sees two distinguishable authenticator
	// entries.
	twoFactorIssuer = "ggscale control panel"

	msgTwoFactorUnavailable = "Two-factor authentication is not available on this server."
	msgTwoFactorBadCode     = "That code is incorrect."
	msgTwoFactorLocked      = "Too many attempts. Try again later."
	// msgTwoFactorBroken covers a secret that no configured key can open —
	// the operator changed or removed key material after enrollments.
	msgTwoFactorBroken = "Two-factor authentication is temporarily unavailable. Contact your operator."
)

// These alias the shared twofactor verify sentinels so the challenge and
// account-page error mapping stays unchanged while the security-sensitive
// verify logic itself lives once in the twofactor package.
var (
	errTwoFactorBadCode     = twofactor.ErrBadCode
	errTwoFactorLocked      = twofactor.ErrLocked
	errTwoFactorUnavailable = twofactor.ErrUnavailable
)

// finishLogin is the single post-password gate. Both the login POST and the
// email-verify POST land here, so the TOTP challenge cannot be bypassed by
// finishing a login through the verify path.
func (h *Handler) finishLogin(w http.ResponseWriter, r *http.Request, user controlPanelUser) {
	row, found, err := h.getTOTP(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "two-factor lookup failed", http.StatusInternalServerError)
		return
	}
	if found && row.ConfirmedAt.Valid && !h.trustedDeviceOK(r, user.ID) {
		if h.twoFactor == nil {
			// Fail closed: an enrolled account never logs in without its
			// second factor, even when the operator removed the key.
			w.WriteHeader(http.StatusServiceUnavailable)
			webutil.Render(r, w, LoginPage(LoginView{Email: user.Email, Error: "Two-factor authentication is unavailable on this server. Contact your operator."}))
			return
		}
		h.setTwoFactorPendingCookie(w, user)
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginTwoFactorRequired)
		htmxRedirect(w, r, pathControlPanelLogin2FA)
		return
	}
	h.completeLogin(w, r, user, nil)
}

// completeLogin writes the login audit row, mints the session, and lands
// the user on the control panel.
func (h *Handler) completeLogin(w http.ResponseWriter, r *http.Request, user controlPanelUser, auditPayload any) {
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		return auditlog.WritePlatform(r.Context(), tx, user.ID, "control_panel.login", user.Email, auditPayload)
	}); err != nil {
		http.Error(w, "audit log failed", http.StatusInternalServerError)
		return
	}
	if _, err := h.issueSession(r.Context(), w, user.ID, h.clientIP(r), r.UserAgent()); err != nil {
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	h.metrics.Login(observability.SurfaceControlPanel, observability.LoginOK)
	htmxRedirect(w, r, pathControlPanel)
}

func (h *Handler) setTwoFactorPendingCookie(w http.ResponseWriter, user controlPanelUser) {
	value := twofactor.EncodePending(h.twoFactor.PendingKey(), twofactor.Pending{
		Subject:   strconv.FormatInt(user.ID, 10),
		Email:     user.Email,
		ExpiresAt: h.now().Add(twofactor.PendingTTL).Unix(),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     twoFactorPendingCookieName,
		Value:    value,
		Path:     pathControlPanel,
		MaxAge:   int(twofactor.PendingTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearTwoFactorPendingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     twoFactorPendingCookieName,
		Value:    "",
		Path:     pathControlPanel,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) twoFactorPendingFromRequest(r *http.Request) (controlPanelUser, bool) {
	if h.twoFactor == nil {
		return controlPanelUser{}, false
	}
	c, err := r.Cookie(twoFactorPendingCookieName)
	if err != nil {
		return controlPanelUser{}, false
	}
	p, ok := twofactor.DecodePending(h.twoFactor.PendingKey(), c.Value, h.now())
	if !ok {
		return controlPanelUser{}, false
	}
	id, err := strconv.ParseInt(p.Subject, 10, 64)
	if err != nil {
		return controlPanelUser{}, false
	}
	return controlPanelUser{ID: id, Email: p.Email}, true
}

func (h *Handler) getTOTP(ctx context.Context, userID int64) (sqlcgen.GetControlPanelTOTPRow, bool, error) {
	var row sqlcgen.GetControlPanelTOTPRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetControlPanelTOTP(ctx, userID)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	return row, true, nil
}

func (h *Handler) trustedDeviceOK(r *http.Request, userID int64) bool {
	c, err := r.Cookie(trustedDeviceCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	hash := sha256.Sum256([]byte(c.Value))
	found := false
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).GetControlPanelTrustedDevice(r.Context(), sqlcgen.GetControlPanelTrustedDeviceParams{
			TokenHash:          hash[:],
			ControlPanelUserID: userID,
		})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		found = qerr == nil
		return qerr
	})
	return err == nil && found
}

func (h *Handler) mintTrustedDevice(ctx context.Context, w http.ResponseWriter, userID int64, ip, userAgent string) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	expires := h.now().Add(twofactor.TrustedDeviceTTL)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).CreateControlPanelTrustedDevice(ctx, sqlcgen.CreateControlPanelTrustedDeviceParams{
			ControlPanelUserID: userID,
			TokenHash:          hash[:],
			ExpiresAt:          pgtype.Timestamptz{Time: expires, Valid: true},
			Ip:                 optionalString(ip),
			UserAgent:          optionalString(userAgent),
		})
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     trustedDeviceCookieName,
		Value:    token,
		Path:     pathControlPanel,
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return nil
}

func (h *Handler) deleteTrustedDevices(ctx context.Context, tx pgx.Tx, userID int64) error {
	return sqlcgen.New(tx).DeleteControlPanelTrustedDevicesForUser(ctx, userID)
}

// reserveTOTPAttempt atomically consumes one challenge attempt, tipping the
// row into lockout at the cap. errTwoFactorLocked when already at cap.
func (h *Handler) reserveTOTPAttempt(ctx context.Context, userID int64) error {
	lockoutUntil := pgtype.Timestamptz{Time: h.now().Add(twofactor.LockoutDuration), Valid: true}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ReserveControlPanelTOTPAttempt(ctx, sqlcgen.ReserveControlPanelTOTPAttemptParams{
			ControlPanelUserID: userID,
			MaxAttempts:        int32(twofactor.MaxAttempts),
			LockoutUntil:       lockoutUntil,
		})
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errTwoFactorLocked
	}
	return err
}

func (h *Handler) resetTOTPAttempts(ctx context.Context, userID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).ResetControlPanelTOTPAttempts(ctx, userID)
	})
}

// verifyTwoFactorCode validates a TOTP or backup code against the confirmed
// credential under the shared attempt cap, returning the method used
// ("totp" | "backup_code"). The security-sensitive flow (lockout, replay,
// attempt accounting) lives in twofactor.Verify; this binds it to the
// control panel user's persistence.
func (h *Handler) verifyTwoFactorCode(ctx context.Context, userID int64, code string, allowBackup bool) (string, error) {
	return twofactor.Verify(ctx, h.twoFactor, controlPanelTOTPStore{h: h, userID: userID}, code, h.now(), allowBackup)
}

// controlPanelTOTPStore adapts one control panel user's TOTP persistence to
// twofactor.Store.
type controlPanelTOTPStore struct {
	h      *Handler
	userID int64
}

func (s controlPanelTOTPStore) Credential(ctx context.Context) (twofactor.Credential, bool, error) {
	row, found, err := s.h.getTOTP(ctx, s.userID)
	if err != nil || !found {
		return twofactor.Credential{}, found, err
	}
	return twofactor.Credential{
		SecretEnc:   row.SecretEnc,
		Confirmed:   row.ConfirmedAt.Valid,
		Locked:      row.LockedUntil.Valid,
		LockedUntil: row.LockedUntil.Time,
	}, true, nil
}

func (s controlPanelTOTPStore) ReserveAttempt(ctx context.Context) error {
	return s.h.reserveTOTPAttempt(ctx, s.userID)
}

func (s controlPanelTOTPStore) ResetAttempts(ctx context.Context) error {
	return s.h.resetTOTPAttempts(ctx, s.userID)
}

func (s controlPanelTOTPStore) SetLastUsedStep(ctx context.Context, step int64) (int64, error) {
	var rows int64
	err := s.h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).SetControlPanelTOTPLastUsedStep(ctx, sqlcgen.SetControlPanelTOTPLastUsedStepParams{
			ControlPanelUserID: s.userID,
			LastUsedStep:       step,
		})
		return qerr
	})
	return rows, err
}

func (s controlPanelTOTPStore) ConsumeBackupCode(ctx context.Context, hash []byte) (bool, error) {
	consumed := false
	err := s.h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ConsumeControlPanelTOTPBackupCode(ctx, sqlcgen.ConsumeControlPanelTOTPBackupCodeParams{
			ControlPanelUserID: s.userID,
			CodeHash:           hash,
		})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		consumed = qerr == nil
		return qerr
	})
	return consumed, err
}

func (h *Handler) twoFactorChallengePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.twoFactorPendingFromRequest(r); !ok {
		http.Redirect(w, r, pathControlPanelLogin, http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, TwoFactorChallengePage(TwoFactorChallengeView{}))
}

func (h *Handler) twoFactorChallenge(w http.ResponseWriter, r *http.Request) {
	user, ok := h.twoFactorPendingFromRequest(r)
	if !ok {
		http.Redirect(w, r, pathControlPanelLogin, http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	method, err := h.verifyTwoFactorCode(r.Context(), user.ID, r.Form.Get("code"), true)
	switch {
	case errors.Is(err, errTwoFactorLocked):
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginLocked)
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, TwoFactorChallengePage(TwoFactorChallengeView{Error: msgTwoFactorLocked}))
		return
	case errors.Is(err, errTwoFactorBadCode):
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginInvalid)
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, TwoFactorChallengePage(TwoFactorChallengeView{Error: msgTwoFactorBadCode}))
		return
	case errors.Is(err, errTwoFactorUnavailable):
		w.WriteHeader(http.StatusServiceUnavailable)
		webutil.Render(r, w, TwoFactorChallengePage(TwoFactorChallengeView{Error: msgTwoFactorBroken}))
		return
	case err != nil:
		http.Error(w, "two-factor verification failed", http.StatusInternalServerError)
		return
	}

	// Re-check the account is still active; the query filters disabled rows.
	var row sqlcgen.GetControlPanelUserByEmailRow
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetControlPanelUserByEmail(r.Context(), user.Email)
		return qerr
	})
	if err != nil || row.ID != user.ID {
		h.clearTwoFactorPendingCookie(w)
		http.Redirect(w, r, pathControlPanelLogin, http.StatusSeeOther)
		return
	}
	h.clearTwoFactorPendingCookie(w)
	if r.Form.Get("trust_device") != "" {
		if err := h.mintTrustedDevice(r.Context(), w, user.ID, h.clientIP(r), r.UserAgent()); err != nil {
			// A failed remember-device write shouldn't abort an
			// otherwise-valid login; the user just gets challenged again
			// next time.
			slog.ErrorContext(r.Context(), "trusted device create", "err", err)
		}
	}
	h.completeLogin(w, r, controlPanelUser{ID: row.ID, Email: row.Email, IsPlatformAdmin: row.IsPlatformAdmin}, map[string]string{"method": method})
}

// accountView assembles the account page view, including 2FA status.
func (h *Handler) accountView(ctx context.Context, session controlPanelSession) (AccountView, error) {
	vm := AccountView{
		UserEmail:          session.User.Email,
		CSRFToken:          session.CSRFToken,
		TwoFactorAvailable: h.twoFactor != nil,
	}
	row, found, err := h.getTOTP(ctx, session.User.ID)
	if err != nil {
		return vm, err
	}
	if !found || !row.ConfirmedAt.Valid {
		return vm, nil
	}
	vm.TwoFactorEnabled = true
	var remaining int64
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		remaining, qerr = sqlcgen.New(tx).CountControlPanelTOTPBackupCodesRemaining(ctx, session.User.ID)
		return qerr
	})
	if err != nil {
		return vm, err
	}
	vm.BackupCodesRemaining = int(remaining)
	return vm, nil
}

func (h *Handler) renderAccount(w http.ResponseWriter, r *http.Request, session controlPanelSession, status int, mutate func(*AccountView)) {
	vm, err := h.accountView(r.Context(), session)
	if err != nil {
		http.Error(w, "account lookup failed", http.StatusInternalServerError)
		return
	}
	if mutate != nil {
		mutate(&vm)
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	webutil.Render(r, w, AccountPage(vm))
}

func (h *Handler) twoFactorSetup(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if h.twoFactor == nil {
		h.renderAccount(w, r, session, http.StatusConflict, func(vm *AccountView) { vm.Error = msgTwoFactorUnavailable })
		return
	}
	key, err := twofactor.GenerateKey(twoFactorIssuer, session.User.Email)
	if err != nil {
		http.Error(w, "two-factor setup failed", http.StatusInternalServerError)
		return
	}
	secretEnc, err := h.twoFactor.Encrypt([]byte(key.Secret()))
	if err != nil {
		http.Error(w, "two-factor setup failed", http.StatusInternalServerError)
		return
	}
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).UpsertControlPanelTOTPPending(r.Context(), sqlcgen.UpsertControlPanelTOTPPendingParams{
			ControlPanelUserID: session.User.ID,
			SecretEnc:          secretEnc,
		})
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.renderAccount(w, r, session, http.StatusConflict, func(vm *AccountView) { vm.Error = "Two-factor authentication is already enabled." })
		return
	}
	if err != nil {
		http.Error(w, "two-factor setup failed", http.StatusInternalServerError)
		return
	}
	h.renderTwoFactorSetup(w, r, session, key, http.StatusOK, "")
}

func (h *Handler) renderTwoFactorSetup(w http.ResponseWriter, r *http.Request, session controlPanelSession, key *otp.Key, status int, errMsg string) {
	qr, err := twofactor.QRPNGDataURI(key)
	if err != nil {
		http.Error(w, "two-factor setup failed", http.StatusInternalServerError)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	webutil.Render(r, w, TwoFactorSetupPage(TwoFactorSetupView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		QRDataURI: qr,
		Secret:    twofactor.GroupSecret(key.Secret()),
		Error:     errMsg,
	}))
}

func (h *Handler) twoFactorConfirm(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !webutil.ParseForm(w, r) {
		return
	}
	if h.twoFactor == nil {
		h.renderAccount(w, r, session, http.StatusConflict, func(vm *AccountView) { vm.Error = msgTwoFactorUnavailable })
		return
	}
	row, found, err := h.getTOTP(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "two-factor lookup failed", http.StatusInternalServerError)
		return
	}
	if !found || row.ConfirmedAt.Valid {
		http.Redirect(w, r, pathControlPanelAccount, http.StatusSeeOther)
		return
	}
	secret, err := h.twoFactor.Decrypt(row.SecretEnc)
	if err != nil {
		http.Error(w, "two-factor confirm failed", http.StatusInternalServerError)
		return
	}
	step, ok := twofactor.ValidateCode(string(secret), r.Form.Get("code"), h.now())
	if !ok {
		key, kerr := twofactor.KeyFromParts(twoFactorIssuer, session.User.Email, string(secret))
		if kerr != nil {
			http.Error(w, "two-factor confirm failed", http.StatusInternalServerError)
			return
		}
		h.renderTwoFactorSetup(w, r, session, key, http.StatusUnprocessableEntity, msgTwoFactorBadCode)
		return
	}
	codes, err := twofactor.GenerateBackupCodes()
	if err != nil {
		http.Error(w, "two-factor confirm failed", http.StatusInternalServerError)
		return
	}
	confirmed := false
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rows, qerr := q.ConfirmControlPanelTOTP(r.Context(), sqlcgen.ConfirmControlPanelTOTPParams{
			ControlPanelUserID: session.User.ID,
			LastUsedStep:       step,
		})
		if qerr != nil {
			return qerr
		}
		if rows == 0 {
			return nil
		}
		confirmed = true
		if qerr := h.replaceBackupCodes(r.Context(), q, session.User.ID, codes); qerr != nil {
			return qerr
		}
		if qerr := h.deleteTrustedDevices(r.Context(), tx, session.User.ID); qerr != nil {
			return qerr
		}
		if qerr := q.RevokeOtherControlPanelSessionsForUser(r.Context(), sqlcgen.RevokeOtherControlPanelSessionsForUserParams{
			ControlPanelUserID: session.User.ID,
			KeepSessionID:      session.ID,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.2fa_enable", session.User.Email, nil)
	})
	if err != nil {
		http.Error(w, "two-factor confirm failed", http.StatusInternalServerError)
		return
	}
	if !confirmed {
		http.Redirect(w, r, pathControlPanelAccount, http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, TwoFactorBackupCodesPage(TwoFactorBackupCodesView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Message:   "Two-factor authentication is enabled. Save these backup codes somewhere safe — they are shown only once and are the only way back in if you lose your authenticator.",
		Codes:     codes,
	}))
}

func (h *Handler) replaceBackupCodes(ctx context.Context, q *sqlcgen.Queries, userID int64, codes []string) error {
	if err := q.DeleteControlPanelTOTPBackupCodes(ctx, userID); err != nil {
		return err
	}
	rows := make([]sqlcgen.InsertControlPanelTOTPBackupCodesParams, len(codes))
	for i, code := range codes {
		rows[i] = sqlcgen.InsertControlPanelTOTPBackupCodesParams{
			ControlPanelUserID: userID,
			CodeHash:           twofactor.HashBackupCode(code),
		}
	}
	_, err := q.InsertControlPanelTOTPBackupCodes(ctx, rows)
	return err
}

// checkAccountPassword verifies the acting user's current password for the
// destructive 2FA management actions.
func (h *Handler) checkAccountPassword(ctx context.Context, email, password string) (bool, error) {
	var row sqlcgen.GetControlPanelUserByEmailRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetControlPanelUserByEmail(ctx, email)
		return qerr
	})
	if err != nil {
		// The acting user is logged in, so a missing row is an internal fault,
		// not a wrong password — surface it as such rather than a 401.
		return false, err
	}
	return bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) == nil, nil
}

func (h *Handler) twoFactorDisable(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !webutil.ParseForm(w, r) {
		return
	}
	passwordOK, err := h.checkAccountPassword(r.Context(), session.User.Email, r.Form.Get("current_password"))
	if err != nil {
		http.Error(w, "account lookup failed", http.StatusInternalServerError)
		return
	}
	if !passwordOK {
		h.renderAccount(w, r, session, http.StatusUnauthorized, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{"disable_password": "Current password is incorrect"}
		})
		return
	}
	method, err := h.verifyTwoFactorCode(r.Context(), session.User.ID, r.Form.Get("code"), true)
	if handled := h.renderTwoFactorCodeError(w, r, session, err, "disable_code"); handled {
		return
	}
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if qerr := q.DeleteControlPanelTOTP(r.Context(), session.User.ID); qerr != nil {
			return qerr
		}
		if qerr := h.deleteTrustedDevices(r.Context(), tx, session.User.ID); qerr != nil {
			return qerr
		}
		if qerr := q.RevokeOtherControlPanelSessionsForUser(r.Context(), sqlcgen.RevokeOtherControlPanelSessionsForUserParams{
			ControlPanelUserID: session.User.ID,
			KeepSessionID:      session.ID,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.2fa_disable", session.User.Email, map[string]string{"method": method})
	})
	if err != nil {
		http.Error(w, "two-factor disable failed", http.StatusInternalServerError)
		return
	}
	h.renderAccount(w, r, session, http.StatusOK, func(vm *AccountView) {
		vm.Message = "Two-factor authentication disabled."
	})
}

func (h *Handler) twoFactorRegenerateBackupCodes(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !webutil.ParseForm(w, r) {
		return
	}
	passwordOK, err := h.checkAccountPassword(r.Context(), session.User.Email, r.Form.Get("current_password"))
	if err != nil {
		http.Error(w, "account lookup failed", http.StatusInternalServerError)
		return
	}
	if !passwordOK {
		h.renderAccount(w, r, session, http.StatusUnauthorized, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{"regenerate_password": "Current password is incorrect"}
		})
		return
	}
	// Authenticator code only: someone left with nothing but backup codes
	// should disable and re-enroll, not spend their last code minting more.
	_, err = h.verifyTwoFactorCode(r.Context(), session.User.ID, r.Form.Get("code"), false)
	if handled := h.renderTwoFactorCodeError(w, r, session, err, "regenerate_code"); handled {
		return
	}
	codes, err := twofactor.GenerateBackupCodes()
	if err != nil {
		http.Error(w, "backup code generation failed", http.StatusInternalServerError)
		return
	}
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		if qerr := h.replaceBackupCodes(r.Context(), sqlcgen.New(tx), session.User.ID, codes); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.2fa_backup_codes_regenerate", session.User.Email, nil)
	})
	if err != nil {
		http.Error(w, "backup code regeneration failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, TwoFactorBackupCodesPage(TwoFactorBackupCodesView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Message:   "New backup codes generated. Your old codes no longer work.",
		Codes:     codes,
	}))
}

// renderTwoFactorCodeError maps verifyTwoFactorCode failures onto the
// account page. Returns true when it wrote a response.
func (h *Handler) renderTwoFactorCodeError(w http.ResponseWriter, r *http.Request, session controlPanelSession, err error, field string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errTwoFactorLocked):
		h.renderAccount(w, r, session, http.StatusTooManyRequests, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{field: msgTwoFactorLocked}
		})
	case errors.Is(err, errTwoFactorBadCode):
		h.renderAccount(w, r, session, http.StatusUnauthorized, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{field: msgTwoFactorBadCode}
		})
	case errors.Is(err, errTwoFactorUnavailable):
		h.renderAccount(w, r, session, http.StatusServiceUnavailable, func(vm *AccountView) {
			vm.Error = msgTwoFactorBroken
		})
	default:
		http.Error(w, "two-factor verification failed", http.StatusInternalServerError)
	}
	return true
}
