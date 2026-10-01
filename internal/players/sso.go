package players

// Single sign-on for global player accounts. The protocol mechanics live in
// internal/sso; this file decides what a verified provider identity means
// for the player account store. Every path that ends in a session goes
// through finishAccountLogin, so the disabled check, the two-factor
// challenge, and the trusted-device check are the same as for a password.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/auditlog"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	accountSSOCookieName = "ggscale_account_sso"
	accountSSOPath       = accountBasePath + "/sso"
	accountSSOPurpose    = "player-sso"
	// accountSSOAuditActor is the service actor on platform audit rows:
	// the actor is a player account, which the user column cannot hold.
	accountSSOAuditActor  = "player_sso"
	ssoDisplayNameMaxChar = 64

	msgSSOBadState   = "That sign-in attempt expired or is not valid. Try again."
	msgSSOEmailTaken = "An account with this email exists. Sign in with your password, then link the provider from your account page."
	msgSSORetry      = "Could not complete the sign-in. Try again."
)

var (
	errSSOEmailTaken    = errors.New("players: sso email belongs to an account")
	errSSOLastMethod    = errors.New("players: last sign-in method")
	errSSONotLinked     = errors.New("players: provider not linked")
	errIdentityPassword = errors.New("players: current password incorrect")
)

func newAccountSSOFlow(h *Handler, providers map[string]sso.Provider) *sso.Flow {
	return &sso.Flow{
		Providers:  providers,
		Key:        sso.StateKey(h.verifySigningKey),
		Purpose:    accountSSOPurpose,
		CookieName: accountSSOCookieName,
		CookiePath: accountSSOPath,
		Secure:     h.cfg.CookieSecure,
		Now:        func() time.Time { return h.now() },
	}
}

func (h *Handler) ssoButtons() []SSOProviderView {
	enabled := h.sso.Enabled()
	out := make([]SSOProviderView, 0, len(enabled))
	for _, p := range enabled {
		out = append(out, SSOProviderView{Name: p.Name, Label: p.Label})
	}
	return out
}

// signInMethods is the provider list for the account page, with the link
// state of each provider.
func (h *Handler) signInMethods(ctx context.Context, accountID pgtype.UUID) ([]SSOProviderView, error) {
	out := h.ssoButtons()
	if len(out) == 0 {
		return nil, nil
	}
	var rows []sqlcgen.ListPlayerAccountConnectionsRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).ListPlayerAccountConnections(ctx, accountID)
		return qerr
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		for _, row := range rows {
			if row.Provider == out[i].Name {
				out[i].Linked = true
				out[i].LinkedAt = row.CreatedAt.Time
			}
		}
	}
	return out, nil
}

func (h *Handler) renderAccountLogin(w http.ResponseWriter, r *http.Request, status int, email, errMsg string) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	webutil.Render(r, w, AccountLoginPage(AccountLoginView{
		Email:     email,
		Error:     errMsg,
		CSRFToken: h.csrf(r),
		Providers: h.ssoButtons(),
	}))
}

func redirectAccountHomeError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, accountBasePath+"/?error="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectAccountHomeFlash(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, accountBasePath+"/?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// confirmAccountIdentity is the check before a change to the sign-in
// methods: the current password when the account has one, else a two-factor
// code when two-factor authentication is on, else nothing. A stolen session
// alone must not add a sign-in method that outlives the session.
func (h *Handler) confirmAccountIdentity(ctx context.Context, sess accountSession, form url.Values) error {
	if sess.HasPassword {
		ok, err := h.checkAccountCurrentPassword(ctx, sess.Email, form.Get("current_password"))
		if err != nil {
			return err
		}
		if !ok {
			return errIdentityPassword
		}
		return nil
	}
	accountID := toPgUUID(sess.AccountID)
	row, found, err := h.getAccountTOTP(ctx, accountID)
	if err != nil {
		return err
	}
	if !found || !row.ConfirmedAt.Valid {
		return nil
	}
	return h.verifyAccountTwoFactorCode(ctx, accountID, form.Get("code"), true)
}

// signInMethodChange runs the checks that link and unlink share. It returns
// false when it has written the response.
func (h *Handler) signInMethodChange(w http.ResponseWriter, r *http.Request) (accountSession, string, bool) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, accountBasePath+"/login", http.StatusSeeOther)
		return accountSession{}, "", false
	}
	provider := chi.URLParam(r, "provider")
	if !h.sso.Has(provider) {
		http.NotFound(w, r)
		return accountSession{}, "", false
	}
	if !webutil.ParseForm(w, r) {
		return accountSession{}, "", false
	}
	err := h.confirmAccountIdentity(r.Context(), sess, r.Form)
	switch {
	case err == nil:
		return sess, provider, true
	case errors.Is(err, errIdentityPassword):
		redirectAccountHomeError(w, r, "Current password is incorrect.")
	case errors.Is(err, errTwoFactorBadCode):
		redirectAccountHomeError(w, r, msgTwoFactorBadCode)
	case errors.Is(err, errTwoFactorLocked):
		redirectAccountHomeError(w, r, msgTwoFactorLocked)
	case errors.Is(err, errTwoFactorUnavailable):
		redirectAccountHomeError(w, r, msgTwoFactorBroken)
	default:
		webutil.InternalError(w, "account sso: identity check", err)
	}
	return accountSession{}, "", false
}

func (h *Handler) startAccountSSO(w http.ResponseWriter, r *http.Request, provider, mode, actor string) {
	err := h.sso.Start(w, r, provider, mode, actor, "")
	if errors.Is(err, sso.ErrUnknownProvider) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "account sso: start", err)
	}
}

func (h *Handler) accountSSOStart(w http.ResponseWriter, r *http.Request) {
	h.startAccountSSO(w, r, chi.URLParam(r, "provider"), sso.ModeSignIn, "")
}

func (h *Handler) accountSSOLink(w http.ResponseWriter, r *http.Request) {
	sess, provider, ok := h.signInMethodChange(w, r)
	if !ok {
		return
	}
	h.startAccountSSO(w, r, provider, sso.ModeLink, sess.AccountID.String())
}

func (h *Handler) accountSSOCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	pending, id, err := h.sso.Finish(w, r, provider)
	switch {
	case errors.Is(err, sso.ErrUnknownProvider):
		http.NotFound(w, r)
		return
	case errors.Is(err, sso.ErrBadState):
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		h.renderAccountLogin(w, r, http.StatusBadRequest, "", msgSSOBadState)
		return
	case err != nil:
		msg := "Could not reach " + h.sso.Label(provider) + ". Try again."
		status := http.StatusBadGateway
		if errors.Is(err, sso.ErrDenied) {
			msg = "Sign-in with " + h.sso.Label(provider) + " was cancelled."
			status = http.StatusForbidden
		} else {
			slog.ErrorContext(r.Context(), "player account sso callback", "provider", provider, "err", err)
		}
		if pending.Mode == sso.ModeLink {
			redirectAccountHomeError(w, r, msg)
			return
		}
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		h.renderAccountLogin(w, r, status, "", msg)
		return
	}
	switch pending.Mode {
	case sso.ModeSignIn:
		h.ssoSignIn(w, r, id)
	case sso.ModeLink:
		h.ssoLink(w, r, pending, id)
	default:
		h.renderAccountLogin(w, r, http.StatusBadRequest, "", msgSSOBadState)
	}
}

func (h *Handler) ssoSignIn(w http.ResponseWriter, r *http.Request, id sso.Identity) {
	var row sqlcgen.GetPlayerAccountByConnectionRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		row, qerr = q.GetPlayerAccountByConnection(r.Context(), sqlcgen.GetPlayerAccountByConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		})
		if qerr != nil || row.DisabledAt.Valid {
			return qerr
		}
		return q.TouchPlayerAccountConnection(r.Context(), sqlcgen.TouchPlayerAccountConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		h.ssoSignUp(w, r, id)
		return
	}
	if err != nil {
		webutil.InternalError(w, "account sso: lookup", err)
		return
	}
	if row.DisabledAt.Valid {
		h.metrics.Login(observability.SurfacePlayer, observability.LoginLocked)
		h.renderAccountLogin(w, r, http.StatusForbidden, "", "This account has been disabled.")
		return
	}
	h.finishAccountLogin(w, r, row.ID, row.Email, row.SessionEpoch)
}

// ssoSignUp creates the account on the first sign-in with an unknown
// subject. It never attaches the identity to an account that exists: a
// matching email is rejected, and the owner links the provider from a
// signed-in session.
func (h *Handler) ssoSignUp(w http.ResponseWriter, r *http.Request, id sso.Identity) {
	email := strings.ToLower(strings.TrimSpace(id.Email))
	if !id.EmailVerified || !validEmail(email) {
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		h.renderAccountLogin(w, r, http.StatusForbidden, "",
			"Your "+h.sso.Label(id.Provider)+" account has no verified email address. Verify it there, or sign up with email and password.")
		return
	}
	var accountID pgtype.UUID
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		_, qerr := q.GetPlayerAccountByEmail(r.Context(), email)
		if qerr == nil {
			return errSSOEmailTaken
		}
		if !errors.Is(qerr, pgx.ErrNoRows) {
			return qerr
		}
		accountID, qerr = q.CreateVerifiedPlayerAccount(r.Context(), sqlcgen.CreateVerifiedPlayerAccountParams{
			Email:       email,
			DisplayName: ssoDisplayName(id.Name),
		})
		if qerr != nil {
			return qerr
		}
		if qerr := q.InsertPlayerAccountConnection(r.Context(), sqlcgen.InsertPlayerAccountConnectionParams{
			PlayerAccountID: accountID, Provider: id.Provider, Subject: id.Subject,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatformService(r.Context(), tx, accountSSOAuditActor, "sso.signup",
			fromPgUUID(accountID).String(), map[string]any{"provider": id.Provider})
	})
	switch {
	case errors.Is(err, errSSOEmailTaken):
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		h.renderAccountLogin(w, r, http.StatusConflict, "", msgSSOEmailTaken)
		return
	case webutil.IsUniqueViolation(err):
		// Two callbacks raced on the same email or subject. The retry
		// finds the row the winner made and takes the correct branch.
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		h.renderAccountLogin(w, r, http.StatusConflict, "", msgSSORetry)
		return
	case err != nil:
		webutil.InternalError(w, "account sso: signup", err)
		return
	}
	h.metrics.Signup(observability.SignupAccount)
	h.finishAccountLogin(w, r, accountID, email, 0)
}

// ssoDisplayName seeds the display name from the provider profile. A name
// that does not fit the display-name rules is dropped, not repaired.
func ssoDisplayName(name string) *string {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > ssoDisplayNameMaxChar {
		return nil
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return nil
		}
	}
	return &name
}

// linkOutcome is the result of a link transaction that committed.
type linkOutcome int

const (
	linkDone linkOutcome = iota
	linkAlready
	linkSubjectTaken
	linkProviderTaken
)

func (h *Handler) ssoLink(w http.ResponseWriter, r *http.Request, pending sso.Pending, id sso.Identity) {
	sess, ok := h.accountSessionFromRequest(r)
	if !ok || sess.AccountID.String() != pending.Actor {
		h.renderAccountLogin(w, r, http.StatusForbidden, "", "Sign in, then start the link again.")
		return
	}
	outcome, err := h.linkAccountConnection(r.Context(), toPgUUID(sess.AccountID), id)
	label := h.sso.Label(id.Provider)
	switch {
	case webutil.IsUniqueViolation(err):
		redirectAccountHomeError(w, r, msgSSORetry)
	case err != nil:
		webutil.InternalError(w, "account sso: link", err)
	case outcome == linkAlready:
		redirectAccountHomeFlash(w, r, "This "+label+" account is already linked.")
	case outcome == linkSubjectTaken:
		redirectAccountHomeError(w, r, "This "+label+" account cannot be linked.")
	case outcome == linkProviderTaken:
		redirectAccountHomeError(w, r, "Your account already has a "+label+" account linked. Unlink it first.")
	default:
		redirectAccountHomeFlash(w, r, label+" linked.")
	}
}

func (h *Handler) linkAccountConnection(ctx context.Context, accountID pgtype.UUID, id sso.Identity) (linkOutcome, error) {
	outcome := linkDone
	target := fromPgUUID(accountID).String()
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		holder, qerr := q.GetPlayerAccountByConnection(ctx, sqlcgen.GetPlayerAccountByConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		})
		switch {
		case qerr == nil && holder.ID == accountID:
			outcome = linkAlready
			return nil
		case qerr == nil:
			// The rejection commits with its audit row, so the operator
			// can find the two accounts. The browser learns nothing
			// about the other account.
			outcome = linkSubjectTaken
			return auditlog.WritePlatformService(ctx, tx, accountSSOAuditActor, "sso.link_collision", target,
				map[string]any{"provider": id.Provider, "linked_account_id": fromPgUUID(holder.ID).String()})
		case !errors.Is(qerr, pgx.ErrNoRows):
			return qerr
		}
		connections, qerr := q.ListPlayerAccountConnections(ctx, accountID)
		if qerr != nil {
			return qerr
		}
		for _, c := range connections {
			if c.Provider == id.Provider {
				outcome = linkProviderTaken
				return nil
			}
		}
		if qerr := q.InsertPlayerAccountConnection(ctx, sqlcgen.InsertPlayerAccountConnectionParams{
			PlayerAccountID: accountID, Provider: id.Provider, Subject: id.Subject,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatformService(ctx, tx, accountSSOAuditActor, "sso.link", target,
			map[string]any{"provider": id.Provider})
	})
	return outcome, err
}

func (h *Handler) accountSSOUnlink(w http.ResponseWriter, r *http.Request) {
	sess, provider, ok := h.signInMethodChange(w, r)
	if !ok {
		return
	}
	accountID := toPgUUID(sess.AccountID)
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// The row lock makes two unlink requests run one after the other,
		// so they cannot both remove what each sees as a spare method.
		hasPassword, qerr := q.LockPlayerAccountSignInMethods(r.Context(), accountID)
		if qerr != nil {
			return qerr
		}
		removed, qerr := q.DeletePlayerAccountConnection(r.Context(), sqlcgen.DeletePlayerAccountConnectionParams{
			PlayerAccountID: accountID, Provider: provider,
		})
		if qerr != nil {
			return qerr
		}
		if removed == 0 {
			return errSSONotLinked
		}
		remaining, qerr := q.CountPlayerAccountConnections(r.Context(), accountID)
		if qerr != nil {
			return qerr
		}
		if !hasPassword && remaining == 0 {
			return errSSOLastMethod
		}
		return auditlog.WritePlatformService(r.Context(), tx, accountSSOAuditActor, "sso.unlink",
			sess.AccountID.String(), map[string]any{"provider": provider})
	})
	label := h.sso.Label(provider)
	switch {
	case errors.Is(err, errSSONotLinked):
		redirectAccountHomeError(w, r, label+" is not linked.")
	case errors.Is(err, errSSOLastMethod):
		redirectAccountHomeError(w, r, label+" is your only sign-in method. Set a password or link a different provider first.")
	case err != nil:
		webutil.InternalError(w, "account sso: unlink", err)
	default:
		redirectAccountHomeFlash(w, r, label+" unlinked.")
	}
}
