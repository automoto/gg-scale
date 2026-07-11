package players

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pquerna/otp"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	accountTwoFactorCookieName = "ggscale_account_2fa"
	accountTrustCookieName     = "ggscale_account_trust"
	accountChallengePath       = accountBasePath + "/login/2fa"
	accountTwoFactorPath       = accountBasePath + "/2fa"
	// playerTwoFactorIssuer is distinct from the control panel issuer so someone
	// enrolled on both surfaces sees two distinguishable authenticator
	// entries.
	playerTwoFactorIssuer = "ggscale"

	msgTwoFactorUnavailable = "Two-factor authentication is not available on this server."
	msgTwoFactorBadCode     = "That code is incorrect."
	msgTwoFactorLocked      = "Too many attempts. Try again later."
	// msgTwoFactorBroken covers a secret that no configured key can open —
	// the operator changed or removed key material after enrollments.
	msgTwoFactorBroken = "Two-factor authentication is temporarily unavailable. Contact support."
)

// These alias the shared twofactor verify sentinels so the challenge and
// account-page error mapping stays unchanged while the security-sensitive
// verify logic itself lives once in the twofactor package.
var (
	errTwoFactorBadCode     = twofactor.ErrBadCode
	errTwoFactorLocked      = twofactor.ErrLocked
	errTwoFactorUnavailable = twofactor.ErrUnavailable
)

// finishAccountLogin is the single post-password gate: the login POST and
// the email-verify POST both land here, so the TOTP challenge cannot be
// bypassed by finishing a login through the verify path.
func (h *Handler) finishAccountLogin(w http.ResponseWriter, r *http.Request, accountID pgtype.UUID, email string, epoch int32) {
	row, found, err := h.getAccountTOTP(r.Context(), accountID)
	if err != nil {
		webutil.InternalError(w, "account login: two-factor lookup", err)
		return
	}
	if found && row.ConfirmedAt.Valid && !h.trustedAccountDeviceOK(r, accountID) {
		if h.twoFactor == nil {
			// Fail closed: an enrolled account never logs in without its
			// second factor, even when the operator removed the key.
			w.WriteHeader(http.StatusServiceUnavailable)
			webutil.Render(r, w, AccountLoginPage(AccountLoginView{Email: email, Error: "Two-factor authentication is unavailable on this server. Contact support.", CSRFToken: h.csrf(r)}))
			return
		}
		h.setAccountTwoFactorCookie(w, fromPgUUID(accountID), email)
		h.metrics.Login(observability.SurfacePlayer, observability.LoginTwoFactorRequired)
		http.Redirect(w, r, accountChallengePath, http.StatusSeeOther)
		return
	}
	if err := h.issueAccountSession(r.Context(), w, accountID, epoch); err != nil {
		webutil.InternalError(w, "account login: session", err)
		return
	}
	h.metrics.Login(observability.SurfacePlayer, observability.LoginOK)
	http.Redirect(w, r, accountBasePath+"/", http.StatusSeeOther)
}

func (h *Handler) setAccountTwoFactorCookie(w http.ResponseWriter, accountID uuid.UUID, email string) {
	value := twofactor.EncodePending(h.twoFactor.PendingKey(), twofactor.Pending{
		Subject:   accountID.String(),
		Email:     email,
		ExpiresAt: h.now().Add(twofactor.PendingTTL).Unix(),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     accountTwoFactorCookieName,
		Value:    value,
		Path:     accountBasePath,
		MaxAge:   int(twofactor.PendingTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearAccountTwoFactorCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     accountTwoFactorCookieName,
		Value:    "",
		Path:     accountBasePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) accountTwoFactorPending(r *http.Request) (uuid.UUID, string, bool) {
	if h.twoFactor == nil {
		return uuid.UUID{}, "", false
	}
	c, err := r.Cookie(accountTwoFactorCookieName)
	if err != nil {
		return uuid.UUID{}, "", false
	}
	p, ok := twofactor.DecodePending(h.twoFactor.PendingKey(), c.Value, h.now())
	if !ok {
		return uuid.UUID{}, "", false
	}
	id, err := uuid.Parse(p.Subject)
	if err != nil {
		return uuid.UUID{}, "", false
	}
	return id, p.Email, true
}

func (h *Handler) getAccountTOTP(ctx context.Context, accountID pgtype.UUID) (sqlcgen.GetPlayerAccountTOTPRow, bool, error) {
	var row sqlcgen.GetPlayerAccountTOTPRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountTOTP(ctx, accountID)
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

func (h *Handler) trustedAccountDeviceOK(r *http.Request, accountID pgtype.UUID) bool {
	c, err := r.Cookie(accountTrustCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	hash := sha256.Sum256([]byte(c.Value))
	found := false
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).GetPlayerAccountTrustedDevice(r.Context(), sqlcgen.GetPlayerAccountTrustedDeviceParams{
			TokenHash:       hash[:],
			PlayerAccountID: accountID,
		})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		found = qerr == nil
		return qerr
	})
	return err == nil && found
}

func (h *Handler) mintAccountTrustedDevice(ctx context.Context, w http.ResponseWriter, accountID pgtype.UUID) error {
	token, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(token))
	expires := h.now().Add(twofactor.TrustedDeviceTTL)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).CreatePlayerAccountTrustedDevice(ctx, sqlcgen.CreatePlayerAccountTrustedDeviceParams{
			PlayerAccountID: accountID,
			TokenHash:       hash[:],
			ExpiresAt:       pgtype.Timestamptz{Time: expires, Valid: true},
		})
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     accountTrustCookieName,
		Value:    token,
		Path:     accountBasePath,
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return nil
}

func (h *Handler) reserveAccountTOTPAttempt(ctx context.Context, accountID pgtype.UUID) error {
	lockoutUntil := pgtype.Timestamptz{Time: h.now().Add(twofactor.LockoutDuration), Valid: true}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ReservePlayerAccountTOTPAttempt(ctx, sqlcgen.ReservePlayerAccountTOTPAttemptParams{
			PlayerAccountID: accountID,
			MaxAttempts:     int32(twofactor.MaxAttempts),
			LockoutUntil:    lockoutUntil,
		})
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errTwoFactorLocked
	}
	return err
}

func (h *Handler) resetAccountTOTPAttempts(ctx context.Context, accountID pgtype.UUID) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).ResetPlayerAccountTOTPAttempts(ctx, accountID)
	})
}

// verifyAccountTwoFactorCode validates a TOTP or backup code against the
// confirmed credential under the shared attempt cap. The security-sensitive
// flow (lockout, replay, attempt accounting) lives in twofactor.Verify; this
// binds it to the player account's persistence.
func (h *Handler) verifyAccountTwoFactorCode(ctx context.Context, accountID pgtype.UUID, code string, allowBackup bool) error {
	_, err := twofactor.Verify(ctx, h.twoFactor, accountTOTPStore{h: h, accountID: accountID}, code, h.now(), allowBackup)
	return err
}

// accountTOTPStore adapts one player account's TOTP persistence to
// twofactor.Store.
type accountTOTPStore struct {
	h         *Handler
	accountID pgtype.UUID
}

func (s accountTOTPStore) Credential(ctx context.Context) (twofactor.Credential, bool, error) {
	row, found, err := s.h.getAccountTOTP(ctx, s.accountID)
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

func (s accountTOTPStore) ReserveAttempt(ctx context.Context) error {
	return s.h.reserveAccountTOTPAttempt(ctx, s.accountID)
}

func (s accountTOTPStore) ResetAttempts(ctx context.Context) error {
	return s.h.resetAccountTOTPAttempts(ctx, s.accountID)
}

func (s accountTOTPStore) SetLastUsedStep(ctx context.Context, step int64) (int64, error) {
	var rows int64
	err := s.h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).SetPlayerAccountTOTPLastUsedStep(ctx, sqlcgen.SetPlayerAccountTOTPLastUsedStepParams{
			PlayerAccountID: s.accountID,
			LastUsedStep:    step,
		})
		return qerr
	})
	return rows, err
}

func (s accountTOTPStore) ConsumeBackupCode(ctx context.Context, hash []byte) (bool, error) {
	consumed := false
	err := s.h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).ConsumePlayerAccountTOTPBackupCode(ctx, sqlcgen.ConsumePlayerAccountTOTPBackupCodeParams{
			PlayerAccountID: s.accountID,
			CodeHash:        hash,
		})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return nil
		}
		consumed = qerr == nil
		return qerr
	})
	return consumed, err
}

// --- challenge --------------------------------------------------------------

func (h *Handler) accountTwoFactorChallengePage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.accountTwoFactorPending(r); !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, AccountTwoFactorChallengePage(AccountTwoFactorChallengeView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountTwoFactorChallenge(w http.ResponseWriter, r *http.Request) {
	accountID, email, ok := h.accountTwoFactorPending(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	err := h.verifyAccountTwoFactorCode(r.Context(), toPgUUID(accountID), r.Form.Get("code"), true)
	switch {
	case errors.Is(err, errTwoFactorLocked):
		h.metrics.Login(observability.SurfacePlayer, observability.LoginLocked)
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, AccountTwoFactorChallengePage(AccountTwoFactorChallengeView{Error: msgTwoFactorLocked, CSRFToken: h.csrf(r)}))
		return
	case errors.Is(err, errTwoFactorBadCode):
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, AccountTwoFactorChallengePage(AccountTwoFactorChallengeView{Error: msgTwoFactorBadCode, CSRFToken: h.csrf(r)}))
		return
	case errors.Is(err, errTwoFactorUnavailable):
		w.WriteHeader(http.StatusServiceUnavailable)
		webutil.Render(r, w, AccountTwoFactorChallengePage(AccountTwoFactorChallengeView{Error: msgTwoFactorBroken, CSRFToken: h.csrf(r)}))
		return
	case err != nil:
		webutil.InternalError(w, "account 2fa challenge", err)
		return
	}

	// Re-check the account is still active before minting the session.
	var row sqlcgen.GetPlayerAccountByEmailRow
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(r.Context(), email)
		return qerr
	})
	if err != nil || fromPgUUID(row.ID) != accountID || row.DisabledAt.Valid {
		h.clearAccountTwoFactorCookie(w)
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	h.clearAccountTwoFactorCookie(w)
	if r.Form.Get("trust_device") != "" {
		if err := h.mintAccountTrustedDevice(r.Context(), w, row.ID); err != nil {
			slog.ErrorContext(r.Context(), "account trusted device create", "err", err)
		}
	}
	if err := h.issueAccountSession(r.Context(), w, row.ID, row.SessionEpoch); err != nil {
		webutil.InternalError(w, "account 2fa: session", err)
		return
	}
	h.metrics.Login(observability.SurfacePlayer, observability.LoginOK)
	http.Redirect(w, r, accountBasePath+"/", http.StatusSeeOther)
}

// --- management -------------------------------------------------------------

func (h *Handler) accountTwoFactorView(r *http.Request, sess accountSession) (AccountTwoFactorView, error) {
	vm := AccountTwoFactorView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Available:    h.twoFactor != nil,
	}
	accountID := toPgUUID(sess.AccountID)
	row, found, err := h.getAccountTOTP(r.Context(), accountID)
	if err != nil {
		return vm, err
	}
	if !found || !row.ConfirmedAt.Valid {
		return vm, nil
	}
	vm.Enabled = true
	var remaining int64
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		remaining, qerr = sqlcgen.New(tx).CountPlayerAccountTOTPBackupCodesRemaining(r.Context(), accountID)
		return qerr
	})
	if err != nil {
		return vm, err
	}
	vm.BackupCodesRemaining = int(remaining)
	return vm, nil
}

func (h *Handler) renderAccountTwoFactor(w http.ResponseWriter, r *http.Request, sess accountSession, status int, mutate func(*AccountTwoFactorView)) {
	vm, err := h.accountTwoFactorView(r, sess)
	if err != nil {
		webutil.InternalError(w, "account 2fa page", err)
		return
	}
	if mutate != nil {
		mutate(&vm)
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	webutil.Render(r, w, AccountTwoFactorPage(vm))
}

func (h *Handler) accountTwoFactorPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	h.renderAccountTwoFactor(w, r, sess, http.StatusOK, nil)
}

func (h *Handler) accountTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if h.twoFactor == nil {
		h.renderAccountTwoFactor(w, r, sess, http.StatusConflict, func(vm *AccountTwoFactorView) { vm.Error = msgTwoFactorUnavailable })
		return
	}
	key, err := twofactor.GenerateKey(playerTwoFactorIssuer, sess.Email)
	if err != nil {
		webutil.InternalError(w, "account 2fa setup", err)
		return
	}
	secretEnc, err := h.twoFactor.Encrypt([]byte(key.Secret()))
	if err != nil {
		webutil.InternalError(w, "account 2fa setup", err)
		return
	}
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).UpsertPlayerAccountTOTPPending(r.Context(), sqlcgen.UpsertPlayerAccountTOTPPendingParams{
			PlayerAccountID: toPgUUID(sess.AccountID),
			SecretEnc:       secretEnc,
		})
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.renderAccountTwoFactor(w, r, sess, http.StatusConflict, func(vm *AccountTwoFactorView) { vm.Error = "Two-factor authentication is already enabled." })
		return
	}
	if err != nil {
		webutil.InternalError(w, "account 2fa setup", err)
		return
	}
	h.renderAccountTwoFactorSetup(w, r, sess.Email, key, http.StatusOK, "")
}

func (h *Handler) renderAccountTwoFactorSetup(w http.ResponseWriter, r *http.Request, accountEmail string, key *otp.Key, status int, errMsg string) {
	qr, err := twofactor.QRPNGDataURI(key)
	if err != nil {
		webutil.InternalError(w, "account 2fa setup", err)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	webutil.Render(r, w, AccountTwoFactorSetupPage(AccountTwoFactorSetupView{
		AccountEmail: accountEmail,
		CSRFToken:    h.csrf(r),
		QRDataURI:    qr,
		Secret:       twofactor.GroupSecret(key.Secret()),
		Error:        errMsg,
	}))
}

func (h *Handler) accountTwoFactorConfirm(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	if h.twoFactor == nil {
		h.renderAccountTwoFactor(w, r, sess, http.StatusConflict, func(vm *AccountTwoFactorView) { vm.Error = msgTwoFactorUnavailable })
		return
	}
	accountID := toPgUUID(sess.AccountID)
	row, found, err := h.getAccountTOTP(r.Context(), accountID)
	if err != nil {
		webutil.InternalError(w, "account 2fa confirm", err)
		return
	}
	if !found || row.ConfirmedAt.Valid {
		http.Redirect(w, r, accountTwoFactorPath, http.StatusSeeOther)
		return
	}
	secret, err := h.twoFactor.Decrypt(row.SecretEnc)
	if err != nil {
		webutil.InternalError(w, "account 2fa confirm", err)
		return
	}
	step, ok := twofactor.ValidateCode(string(secret), r.Form.Get("code"), h.now())
	if !ok {
		key, kerr := twofactor.KeyFromParts(playerTwoFactorIssuer, sess.Email, string(secret))
		if kerr != nil {
			webutil.InternalError(w, "account 2fa confirm", kerr)
			return
		}
		h.renderAccountTwoFactorSetup(w, r, sess.Email, key, http.StatusUnprocessableEntity, msgTwoFactorBadCode)
		return
	}
	codes, err := twofactor.GenerateBackupCodes()
	if err != nil {
		webutil.InternalError(w, "account 2fa confirm", err)
		return
	}
	confirmed := false
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rows, qerr := q.ConfirmPlayerAccountTOTP(r.Context(), sqlcgen.ConfirmPlayerAccountTOTPParams{
			PlayerAccountID: accountID,
			LastUsedStep:    step,
		})
		if qerr != nil {
			return qerr
		}
		if rows == 0 {
			return nil
		}
		confirmed = true
		if qerr := h.replaceAccountBackupCodes(r.Context(), q, accountID, codes); qerr != nil {
			return qerr
		}
		if qerr := q.DeletePlayerAccountTrustedDevicesForAccount(r.Context(), accountID); qerr != nil {
			return qerr
		}
		return h.revokeOtherAccountSessions(r.Context(), q, r, accountID)
	})
	if err != nil {
		webutil.InternalError(w, "account 2fa confirm", err)
		return
	}
	if !confirmed {
		http.Redirect(w, r, accountTwoFactorPath, http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, AccountTwoFactorBackupCodesPage(AccountTwoFactorBackupCodesView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Message:      "Two-factor authentication is enabled. Save these backup codes somewhere safe — they are shown only once and are the only way back in if you lose your authenticator.",
		Codes:        codes,
	}))
}

func (h *Handler) replaceAccountBackupCodes(ctx context.Context, q *sqlcgen.Queries, accountID pgtype.UUID, codes []string) error {
	if err := q.DeletePlayerAccountTOTPBackupCodes(ctx, accountID); err != nil {
		return err
	}
	rows := make([]sqlcgen.InsertPlayerAccountTOTPBackupCodesParams, len(codes))
	for i, code := range codes {
		rows[i] = sqlcgen.InsertPlayerAccountTOTPBackupCodesParams{
			PlayerAccountID: accountID,
			CodeHash:        twofactor.HashBackupCode(code),
		}
	}
	_, err := q.InsertPlayerAccountTOTPBackupCodes(ctx, rows)
	return err
}

// revokeOtherAccountSessions revokes every session except the one attached
// to this request, keyed by the current cookie's refresh hash.
func (h *Handler) revokeOtherAccountSessions(ctx context.Context, q *sqlcgen.Queries, r *http.Request, accountID pgtype.UUID) error {
	c, err := r.Cookie(accountSessionCookieName)
	if err != nil {
		return nil
	}
	hash := sha256.Sum256([]byte(c.Value))
	return q.RevokeOtherPlayerAccountSessions(ctx, sqlcgen.RevokeOtherPlayerAccountSessionsParams{
		PlayerAccountID: accountID,
		KeepRefreshHash: hash[:],
	})
}

func (h *Handler) checkAccountCurrentPassword(ctx context.Context, email, password string) (bool, error) {
	var row sqlcgen.GetPlayerAccountByEmailRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(ctx, email)
		return qerr
	})
	if err != nil {
		// The acting user is logged in, so a missing row is an internal fault,
		// not a wrong password — surface it as such rather than a 401.
		return false, err
	}
	return bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) == nil, nil
}

func (h *Handler) accountTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	passwordOK, err := h.checkAccountCurrentPassword(r.Context(), sess.Email, r.Form.Get("current_password"))
	if err != nil {
		webutil.InternalError(w, "account 2fa disable", err)
		return
	}
	if !passwordOK {
		h.renderAccountTwoFactor(w, r, sess, http.StatusUnauthorized, func(vm *AccountTwoFactorView) {
			vm.FieldErrors = map[string]string{"disable_password": "Current password is incorrect"}
		})
		return
	}
	accountID := toPgUUID(sess.AccountID)
	err = h.verifyAccountTwoFactorCode(r.Context(), accountID, r.Form.Get("code"), true)
	if handled := h.renderAccountTwoFactorCodeError(w, r, sess, err, "disable_code"); handled {
		return
	}
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if qerr := q.DeletePlayerAccountTOTP(r.Context(), accountID); qerr != nil {
			return qerr
		}
		if qerr := q.DeletePlayerAccountTrustedDevicesForAccount(r.Context(), accountID); qerr != nil {
			return qerr
		}
		return h.revokeOtherAccountSessions(r.Context(), q, r, accountID)
	})
	if err != nil {
		webutil.InternalError(w, "account 2fa disable", err)
		return
	}
	h.renderAccountTwoFactor(w, r, sess, http.StatusOK, func(vm *AccountTwoFactorView) {
		vm.Message = "Two-factor authentication disabled."
	})
}

func (h *Handler) accountTwoFactorBackupCodes(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	passwordOK, err := h.checkAccountCurrentPassword(r.Context(), sess.Email, r.Form.Get("current_password"))
	if err != nil {
		webutil.InternalError(w, "account 2fa backup codes", err)
		return
	}
	if !passwordOK {
		h.renderAccountTwoFactor(w, r, sess, http.StatusUnauthorized, func(vm *AccountTwoFactorView) {
			vm.FieldErrors = map[string]string{"regenerate_password": "Current password is incorrect"}
		})
		return
	}
	accountID := toPgUUID(sess.AccountID)
	// Authenticator code only: someone left with nothing but backup codes
	// should disable and re-enroll, not spend their last code minting more.
	err = h.verifyAccountTwoFactorCode(r.Context(), accountID, r.Form.Get("code"), false)
	if handled := h.renderAccountTwoFactorCodeError(w, r, sess, err, "regenerate_code"); handled {
		return
	}
	codes, err := twofactor.GenerateBackupCodes()
	if err != nil {
		webutil.InternalError(w, "account 2fa backup codes", err)
		return
	}
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		return h.replaceAccountBackupCodes(r.Context(), sqlcgen.New(tx), accountID, codes)
	}); err != nil {
		webutil.InternalError(w, "account 2fa backup codes", err)
		return
	}
	webutil.Render(r, w, AccountTwoFactorBackupCodesPage(AccountTwoFactorBackupCodesView{
		AccountEmail: sess.Email,
		CSRFToken:    h.csrf(r),
		Message:      "New backup codes generated. Your old codes no longer work.",
		Codes:        codes,
	}))
}

// renderAccountTwoFactorCodeError maps verifyAccountTwoFactorCode failures
// onto the management page. Returns true when it wrote a response.
func (h *Handler) renderAccountTwoFactorCodeError(w http.ResponseWriter, r *http.Request, sess accountSession, err error, field string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errTwoFactorLocked):
		h.renderAccountTwoFactor(w, r, sess, http.StatusTooManyRequests, func(vm *AccountTwoFactorView) {
			vm.FieldErrors = map[string]string{field: msgTwoFactorLocked}
		})
	case errors.Is(err, errTwoFactorBadCode):
		h.renderAccountTwoFactor(w, r, sess, http.StatusUnauthorized, func(vm *AccountTwoFactorView) {
			vm.FieldErrors = map[string]string{field: msgTwoFactorBadCode}
		})
	case errors.Is(err, errTwoFactorUnavailable):
		h.renderAccountTwoFactor(w, r, sess, http.StatusServiceUnavailable, func(vm *AccountTwoFactorView) {
			vm.Error = msgTwoFactorBroken
		})
	default:
		webutil.InternalError(w, "account 2fa code check", err)
	}
	return true
}
