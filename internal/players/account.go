package players

// Global player-account auth for the player site. Accounts are
// platform-global (no tenant): every query runs through BootstrapQ against
// player_accounts / player_account_sessions. See docs/temp/player-accounts.md.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	accountSessionCookieName = "ggscale_account_session"
	accountVerifyCookieName  = "ggscale_account_verify"
	accountBasePath          = "/v1/players/account"
)

func toPgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
func fromPgUUID(p pgtype.UUID) uuid.UUID {
	return uuid.UUID(p.Bytes)
}

// accountSession is the public view of a signed-in global account.
type accountSession struct {
	AccountID   uuid.UUID
	Email       string
	DisplayName string
}

// --- page handlers ---------------------------------------------------------

func (h *Handler) accountLoginPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, AccountLoginPage(AccountLoginView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountSignupPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, AccountSignupPage(AccountSignupView{CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountVerifyPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, AccountVerifyPage(AccountVerifyView{Email: p.Email, CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountHomePage(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	projects, err := h.listAccountLinkedProjects(r.Context(), sess.AccountID)
	if err != nil {
		webutil.InternalError(w, "account home: linked projects", err)
		return
	}
	webutil.Render(r, w, AccountHomePage(AccountHomeView{
		Email:       sess.Email,
		DisplayName: sess.DisplayName,
		Projects:    projects,
		CSRFToken:   h.csrf(r),
		Flash:       r.URL.Query().Get("flash"),
		FlashError:  r.URL.Query().Get("error"),
	}))
}

var (
	errJoinDisabled   = errors.New("players: public joining disabled for this project")
	errJoinNotFound   = errors.New("players: project not found")
	errJoinOtherOwner = errors.New("players: project identity already linked to another account")
)

// accountJoin links the signed-in account into a project via the public-join
// flow. Allowed only when the effective policy (tenant AND project) permits;
// otherwise the project is invite-only. Idempotent for an already-linked
// account.
func (h *Handler) accountJoin(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	projectID, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("project_id")), 10, 64)
	if err != nil || projectID <= 0 {
		h.redirectAccountHome(w, r, "", "Enter a valid project ID.")
		return
	}
	if err := h.linkAccountToProject(r.Context(), sess, projectID); err != nil {
		switch {
		case errors.Is(err, errJoinDisabled):
			h.redirectAccountHome(w, r, "", "That game is invite-only. Ask the game's team for an invite.")
		case errors.Is(err, errJoinNotFound):
			h.redirectAccountHome(w, r, "", "That game could not be found.")
		case errors.Is(err, errJoinOtherOwner):
			h.redirectAccountHome(w, r, "", "That game profile is already linked to a different account.")
		default:
			webutil.InternalError(w, "account join", err)
		}
		return
	}
	h.redirectAccountHome(w, r, "Joined the game.", "")
}

func (h *Handler) redirectAccountHome(w http.ResponseWriter, r *http.Request, flash, flashErr string) {
	target := accountBasePath + "/"
	switch {
	case flash != "":
		target += "?flash=" + url.QueryEscape(flash)
	case flashErr != "":
		target += "?error=" + url.QueryEscape(flashErr)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// linkAccountToProject enforces the effective public-join policy and links (or
// creates) the account's end_user in the target project.
func (h *Handler) linkAccountToProject(ctx context.Context, sess accountSession, projectID int64) error {
	// Read the effective policy + tenant via the SECURITY DEFINER helper
	// (BootstrapQ: no tenant context yet).
	var (
		tenantID  int64
		effective bool
		found     bool
	)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, effective_enabled FROM project_join_context($1)`, projectID)
		serr := row.Scan(&tenantID, &effective)
		if errors.Is(serr, pgx.ErrNoRows) {
			return nil
		}
		if serr != nil {
			return serr
		}
		found = true
		return nil
	}); err != nil {
		return err
	}
	if !found {
		return errJoinNotFound
	}
	if !effective {
		return errJoinDisabled
	}

	accountUUID := toPgUUID(sess.AccountID)
	tctx := db.WithTenant(ctx, tenantID)
	return h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		emailPtr := &sess.Email
		existing, err := q.GetEndUserForAccountLink(tctx, sqlcgen.GetEndUserForAccountLinkParams{
			ProjectID: projectID,
			Email:     emailPtr,
		})
		switch {
		case err == nil:
			// A row already exists for this email in the project.
			if existing.PlayerAccountID.Valid {
				if existing.PlayerAccountID == accountUUID {
					return nil // idempotent: already linked to this account
				}
				return errJoinOtherOwner
			}
			return q.LinkEndUserToAccount(tctx, sqlcgen.LinkEndUserToAccountParams{
				ID:              existing.ID,
				PlayerAccountID: accountUUID,
			})
		case errors.Is(err, pgx.ErrNoRows):
			externalID, gerr := webutil.RandomHex("user_", 16)
			if gerr != nil {
				return gerr
			}
			_, cerr := q.CreateLinkedEndUser(tctx, sqlcgen.CreateLinkedEndUserParams{
				ProjectID:       projectID,
				ExternalID:      externalID,
				Email:           emailPtr,
				PlayerAccountID: accountUUID,
			})
			return cerr
		default:
			return err
		}
	})
}

// --- mutating handlers -----------------------------------------------------

func (h *Handler) accountSignup(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	displayName := strings.TrimSpace(r.Form.Get("display_name"))
	view := AccountSignupView{Email: email, DisplayName: displayName, CSRFToken: h.csrf(r)}
	if !validEmail(email) {
		view.FieldErrors = map[string]string{"email": "Enter a valid email."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountSignupPage(view))
		return
	}
	if !validPlayerPassword(password) {
		view.FieldErrors = map[string]string{"password": "Password must be between 8 and 72 characters."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountSignupPage(view))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		webutil.InternalError(w, "account signup: bcrypt", err)
		return
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		webutil.InternalError(w, "account signup: code", err)
		return
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		webutil.InternalError(w, "account signup: salt", err)
		return
	}
	var displayPtr *string
	if displayName != "" {
		displayPtr = &displayName
	}

	var accountID pgtype.UUID
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).CreatePlayerAccount(r.Context(), sqlcgen.CreatePlayerAccountParams{
			Email:        email,
			PasswordHash: hash,
			DisplayName:  displayPtr,
			CodeHash:     verifycode.Hash(salt, code),
			CodeSalt:     salt,
			ExpiresAt:    pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
		if qerr != nil {
			return qerr
		}
		accountID = row.ID
		return nil
	})
	if err != nil {
		if webutil.IsUniqueViolation(err) {
			view.Error = "An account with that email already exists. Try logging in."
			w.WriteHeader(http.StatusConflict)
			webutil.Render(r, w, AccountSignupPage(view))
			return
		}
		webutil.InternalError(w, "account signup: insert", err)
		return
	}

	h.sendAccountVerifyEmail(r.Context(), email, code)
	h.setAccountVerifyCookie(w, fromPgUUID(accountID), email)
	http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
}

func (h *Handler) accountLogin(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	if !validPlayerPassword(password) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.renderAccountLoginError(w, r, email)
		return
	}

	var row sqlcgen.GetPlayerAccountByEmailRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountByEmail(r.Context(), email)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.renderAccountLoginError(w, r, email)
		return
	}
	if err != nil {
		webutil.InternalError(w, "account login: lookup", err)
		return
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) != nil {
		h.renderAccountLoginError(w, r, email)
		return
	}
	if row.DisabledAt.Valid {
		w.WriteHeader(http.StatusForbidden)
		webutil.Render(r, w, AccountLoginPage(AccountLoginView{Email: email, Error: "This account has been disabled.", CSRFToken: h.csrf(r)}))
		return
	}
	if !row.EmailVerifiedAt.Valid {
		// Same contract as the dashboard / per-project login: forward to the
		// verify screen with a fresh code. Cooldown (nil) and lockout are not
		// 500s — the verify screen surfaces the lockout on submit.
		if verr := h.startAccountVerification(r.Context(), row.ID, email); verr != nil && !errors.Is(verr, errVerifyAccountLocked) {
			webutil.InternalError(w, "account login: verification email", verr)
			return
		}
		h.setAccountVerifyCookie(w, fromPgUUID(row.ID), email)
		http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
		return
	}
	if err := h.issueAccountSession(r.Context(), w, row.ID, row.SessionEpoch); err != nil {
		webutil.InternalError(w, "account login: session", err)
		return
	}
	http.Redirect(w, r, accountBasePath+"/", http.StatusSeeOther)
}

func (h *Handler) renderAccountLoginError(w http.ResponseWriter, r *http.Request, email string) {
	w.WriteHeader(http.StatusUnauthorized)
	webutil.Render(r, w, AccountLoginPage(AccountLoginView{Email: email, Error: "Invalid email or password.", CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountVerify(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.Form.Get("code"))
	view := AccountVerifyView{Email: p.Email, CSRFToken: h.csrf(r)}
	if len(code) != 6 {
		view.Error = "Enter the 6-digit code from your email."
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	}
	err := h.confirmAccountCode(r.Context(), toPgUUID(p.AccountID), code)
	switch {
	case errors.Is(err, errAlreadyVerified):
		h.clearAccountVerifyCookie(w)
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	case errors.Is(err, errBadVerifyCode):
		view.Error = "That code is incorrect. Try again."
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case errors.Is(err, errVerifyExpired):
		view.Error = "That code has expired. Request a new one."
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case errors.Is(err, errVerifyLocked), errors.Is(err, errVerifyAccountLocked):
		view.Error = "Too many attempts. Request a new code and try again shortly."
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, AccountVerifyPage(view))
		return
	case err != nil:
		webutil.InternalError(w, "account verify", err)
		return
	}
	h.clearAccountVerifyCookie(w)
	// Fetch the (now verified) account for its current epoch before issuing.
	var epoch int32
	if lookupErr := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		acc, qerr := sqlcgen.New(tx).GetPlayerAccountByID(r.Context(), toPgUUID(p.AccountID))
		if qerr != nil {
			return qerr
		}
		epoch = acc.SessionEpoch
		return nil
	}); lookupErr != nil {
		webutil.InternalError(w, "account verify: reload", lookupErr)
		return
	}
	if err := h.issueAccountSession(r.Context(), w, toPgUUID(p.AccountID), epoch); err != nil {
		webutil.InternalError(w, "account verify: session", err)
		return
	}
	http.Redirect(w, r, accountBasePath+"/", http.StatusSeeOther)
}

func (h *Handler) accountVerifyResend(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.accountVerifyCookie(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return
	}
	if err := h.startAccountVerification(r.Context(), toPgUUID(p.AccountID), p.Email); err != nil && !errors.Is(err, errVerifyAccountLocked) {
		webutil.InternalError(w, "account verify resend", err)
		return
	}
	http.Redirect(w, r, accountBasePath+"/verify", http.StatusSeeOther)
}

func (h *Handler) accountLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(accountSessionCookieName); err == nil {
		hash := sha256.Sum256([]byte(c.Value))
		_ = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).RevokePlayerAccountSession(r.Context(), hash[:])
		})
	}
	h.clearAccountSessionCookie(w)
	http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
}

// --- verification helpers (mirror players/dashboard) -----------------------

func (h *Handler) startAccountVerification(ctx context.Context, accountID pgtype.UUID, email string) error {
	var state sqlcgen.GetPlayerAccountVerificationStateRow
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		state, qerr = sqlcgen.New(tx).GetPlayerAccountVerificationState(ctx, accountID)
		return qerr
	}); err != nil {
		return err
	}
	if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
		return errVerifyAccountLocked
	}
	if !verifycode.CanResend(state.EmailVerificationLastSentAt.Time, h.now()) {
		return nil // silent cooldown no-op
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		return err
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		return err
	}
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SetPlayerAccountVerificationCode(ctx, sqlcgen.SetPlayerAccountVerificationCodeParams{
			ID:        accountID,
			CodeHash:  verifycode.Hash(salt, code),
			CodeSalt:  salt,
			ExpiresAt: pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
	}); err != nil {
		return err
	}
	h.sendAccountVerifyEmail(ctx, email, code)
	return nil
}

func (h *Handler) confirmAccountCode(ctx context.Context, accountID pgtype.UUID, code string) error {
	var locked bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		state, err := q.GetPlayerAccountVerificationState(ctx, accountID)
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
		reserved, rerr := q.ReservePlayerAccountVerifyAttempt(ctx, sqlcgen.ReservePlayerAccountVerifyAttemptParams{
			ID:          accountID,
			MaxAttempts: int32(verifycode.MaxAttempts),
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return errVerifyLocked
			}
			return rerr
		}
		if verifycode.LifetimeExhausted(int(reserved.EmailVerificationLifetimeAttempts)) {
			if lerr := q.LockPlayerAccountVerification(ctx, sqlcgen.LockPlayerAccountVerificationParams{
				ID:          accountID,
				LockedUntil: pgtype.Timestamptz{Time: h.now().Add(verifycode.LockoutDuration), Valid: true},
			}); lerr != nil {
				return lerr
			}
			locked = true
			return nil
		}
		expected := verifycode.Hash(state.EmailVerificationSalt, code)
		if subtle.ConstantTimeCompare(expected, state.EmailVerificationCodeHash) != 1 {
			return errBadVerifyCode
		}
		return q.MarkPlayerAccountVerified(ctx, accountID)
	})
	if err != nil {
		return err
	}
	if locked {
		return errVerifyAccountLocked
	}
	return nil
}

func (h *Handler) sendAccountVerifyEmail(ctx context.Context, email, code string) {
	if h.mailer == nil || h.mailFrom == "" {
		return
	}
	_ = h.mailer.Send(ctx, mailer.Message{
		From:    h.mailFrom,
		To:      []string{email},
		Subject: verifySubject,
		Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
	})
}

// --- account sessions ------------------------------------------------------

func (h *Handler) issueAccountSession(ctx context.Context, w http.ResponseWriter, accountID pgtype.UUID, epoch int32) error {
	refreshToken, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(refreshToken))
	expires := h.now().Add(sessionTTL)
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		_, qerr := sqlcgen.New(tx).CreatePlayerAccountSession(ctx, sqlcgen.CreatePlayerAccountSessionParams{
			PlayerAccountID: accountID,
			RefreshHash:     hash[:],
			SessionEpoch:    epoch,
			ExpiresAt:       pgtype.Timestamptz{Time: expires, Valid: true},
		})
		return qerr
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     accountSessionCookieName,
		Value:    refreshToken,
		Path:     "/v1/players",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return nil
}

func (h *Handler) accountSessionFromRequest(r *http.Request) (accountSession, bool) {
	c, err := r.Cookie(accountSessionCookieName)
	if err != nil {
		return accountSession{}, false
	}
	hash := sha256.Sum256([]byte(c.Value))
	var row sqlcgen.GetPlayerAccountSessionRow
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAccountSession(r.Context(), hash[:])
		return qerr
	}); err != nil {
		return accountSession{}, false
	}
	// Reject revoked, expired, disabled, or epoch-stale sessions. The epoch
	// check is the account-level revocation lever: a password change / disable
	// bumps player_accounts.session_epoch past the session's snapshot.
	if row.RevokedAt.Valid || row.ExpiresAt.Time.Before(h.now()) || row.DisabledAt.Valid {
		return accountSession{}, false
	}
	if row.SnapshotEpoch != row.AccountEpoch {
		return accountSession{}, false
	}
	out := accountSession{AccountID: fromPgUUID(row.PlayerAccountID), Email: row.Email}
	if row.DisplayName != nil {
		out.DisplayName = *row.DisplayName
	}
	return out, true
}

func (h *Handler) clearAccountSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     accountSessionCookieName,
		Value:    "",
		Path:     "/v1/players",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

// listAccountLinkedProjects reads the SECURITY DEFINER helper via raw SQL
// (sqlc can't resolve the table-function's columns). Same pattern as
// player_end_user_tenant in issueSession.
func (h *Handler) listAccountLinkedProjects(ctx context.Context, accountID uuid.UUID) ([]LinkedProject, error) {
	var out []LinkedProject
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx,
			`SELECT tenant_id, project_id, project_name, external_id
			 FROM player_account_linked_projects($1)`, toPgUUID(accountID))
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var lp LinkedProject
			if scanErr := rows.Scan(&lp.TenantID, &lp.ProjectID, &lp.ProjectName, &lp.ExternalID); scanErr != nil {
				return scanErr
			}
			out = append(out, lp)
		}
		return rows.Err()
	})
	return out, err
}

// --- verify-pending cookie (account variant) -------------------------------

type accountVerifyPayload struct {
	AccountID uuid.UUID
	Email     string
	ExpiresAt int64
}

func (h *Handler) setAccountVerifyCookie(w http.ResponseWriter, accountID uuid.UUID, email string) {
	expiresAt := h.now().Add(playerVerifyTTL).Unix()
	val := encodeVerifyCookie(verifyCookiePayload{
		AccountID: accountID.String(),
		ExpiresAt: expiresAt,
		Email:     email,
	}, h.verifySigningKey)
	http.SetCookie(w, &http.Cookie{
		Name:     accountVerifyCookieName,
		Value:    val,
		Path:     accountBasePath,
		MaxAge:   int(playerVerifyTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearAccountVerifyCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     accountVerifyCookieName,
		Value:    "",
		Path:     accountBasePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) accountVerifyCookie(r *http.Request) (accountVerifyPayload, bool) {
	c, err := r.Cookie(accountVerifyCookieName)
	if err != nil {
		return accountVerifyPayload{}, false
	}
	p, ok := decodeVerifyCookie(c.Value, h.verifySigningKey)
	if !ok {
		return accountVerifyPayload{}, false
	}
	if p.ExpiresAt > 0 && h.now().Unix() > p.ExpiresAt {
		return accountVerifyPayload{}, false
	}
	id, err := uuid.Parse(p.AccountID)
	if err != nil {
		return accountVerifyPayload{}, false
	}
	return accountVerifyPayload{AccountID: id, Email: p.Email, ExpiresAt: p.ExpiresAt}, true
}
