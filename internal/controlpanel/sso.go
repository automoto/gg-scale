package controlpanel

// Single sign-on for control panel users. The protocol mechanics live in
// internal/sso; this file decides what a verified provider identity means
// for the control panel user store. A provider identity signs in to a user
// it is linked to, and it can create a user when the person accepts an
// emailed invite or tenant signup approval. It never creates a user from
// the login page. Every path that ends in a session goes through
// finishLogin, so the two-factor challenge is the same as for a password.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	ssoCookieName       = "ggscale_control_panel_sso"
	ssoPurpose          = "control-panel-sso"
	pathControlPanelSSO = pathControlPanel + "/sso"
	pathInviteAccept    = pathControlPanel + "/invite/accept"
	pathSignupAccept    = pathControlPanel + "/request-access/accept"
	ssoNoticeParam      = "sso"
)

var (
	// errSSOAcceptNotLinked refuses an invite for a user that exists when
	// the provider identity is not linked to that user. It is the provider
	// form of the current-password rule: the emailed link alone must not
	// open an existing account.
	errSSOAcceptNotLinked = errors.New("control panel: provider identity not linked to the invited user")
	errSSOSubjectTaken    = errors.New("control panel: provider identity linked to a different user")
	errSSOLastMethod      = errors.New("control panel: last sign-in method")
	errSSONotLinked       = errors.New("control panel: provider not linked")
	errIdentityPassword   = errors.New("control panel: current password incorrect")
)

// ssoNotice is a fixed message that the callback selects by key. The
// callback ends in a redirect, and the key travels in the query string, so
// a crafted link can select a message but cannot write one.
type ssoNotice struct {
	text    string
	success bool
}

const (
	noticeState         = "state"
	noticeDenied        = "denied"
	noticeProvider      = "provider"
	noticeNoAccount     = "no_account"
	noticeAcceptNoLink  = "accept_not_linked"
	noticeSubjectTaken  = "subject_taken"
	noticeProviderTaken = "provider_taken"
	noticeLinkSession   = "link_session"
	noticeLinked        = "linked"
	noticeAlreadyLinked = "already_linked"
	noticeNameTaken     = "name_taken"
	noticeFailed        = "failed"
)

var ssoNotices = map[string]ssoNotice{
	noticeState:         {text: "That sign-in attempt expired or is not valid. Try again."},
	noticeDenied:        {text: "The sign-in with the provider was cancelled."},
	noticeProvider:      {text: "Could not reach the sign-in provider. Try again."},
	noticeNoAccount:     {text: "No control panel account is linked to this provider account. Sign in with your password, then link it in account settings."},
	noticeAcceptNoLink:  {text: "This provider account is not linked to your control panel account. Enter your password to continue."},
	noticeSubjectTaken:  {text: "This provider account cannot be used here."},
	noticeProviderTaken: {text: "Your account already has an account of this provider linked. Unlink it first."},
	noticeLinkSession:   {text: "Sign in, then start the link again."},
	noticeNameTaken:     {text: "That Account Tenant name is no longer available. Contact your platform admin."},
	noticeFailed:        {text: "Could not complete the sign-in. Try again."},
	noticeLinked:        {text: "Provider account linked.", success: true},
	noticeAlreadyLinked: {text: "This provider account is already linked.", success: true},
}

// ssoNoticeFromRequest returns the error and success message that the
// callback selected, if any.
func ssoNoticeFromRequest(r *http.Request) (errMsg, okMsg string) {
	notice, found := ssoNotices[r.URL.Query().Get(ssoNoticeParam)]
	switch {
	case !found:
		return "", ""
	case notice.success:
		return "", notice.text
	default:
		return notice.text, ""
	}
}

func redirectWithNotice(w http.ResponseWriter, r *http.Request, path, notice string, query url.Values) {
	if query == nil {
		query = url.Values{}
	}
	query.Set(ssoNoticeParam, notice)
	http.Redirect(w, r, path+"?"+query.Encode(), http.StatusSeeOther)
}

func newSSOFlow(h *Handler, providers map[string]sso.Provider) *sso.Flow {
	return &sso.Flow{
		Providers:  providers,
		Key:        sso.StateKey(h.verifySigningKey),
		Purpose:    ssoPurpose,
		CookieName: ssoCookieName,
		CookiePath: pathControlPanelSSO,
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
func (h *Handler) signInMethods(ctx context.Context, userID int64) ([]SSOProviderView, error) {
	out := h.ssoButtons()
	if len(out) == 0 {
		return nil, nil
	}
	var rows []sqlcgen.ListControlPanelUserConnectionsRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		rows, qerr = sqlcgen.New(tx).ListControlPanelUserConnections(ctx, userID)
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

func (h *Handler) startSSO(w http.ResponseWriter, r *http.Request, mode, actor, code string) {
	err := h.sso.Start(w, r, chi.URLParam(r, "provider"), mode, actor, code)
	if errors.Is(err, sso.ErrUnknownProvider) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "control panel sso start", "err", err)
		http.Error(w, "sign-in start failed", http.StatusInternalServerError)
	}
}

func (h *Handler) ssoLoginStart(w http.ResponseWriter, r *http.Request) {
	h.startSSO(w, r, sso.ModeSignIn, "", "")
}

// ssoInviteStart checks the emailed code before the round trip, so a dead
// invite fails on the page the person is on.
func (h *Handler) ssoInviteStart(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	code := r.Form.Get("code")
	if _, err := h.lookupInvite(r.Context(), code); err != nil {
		h.renderInviteLookupError(w, r, err)
		return
	}
	h.startSSO(w, r, sso.ModeInvite, "", code)
}

func (h *Handler) ssoSignupAcceptStart(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	code := r.Form.Get("code")
	if _, err := h.lookupSignupRequest(r.Context(), code); err != nil {
		h.renderSignupAcceptError(w, r, err)
		return
	}
	h.startSSO(w, r, sso.ModeSignupAccept, "", code)
}

// confirmIdentity is the check before a change to the sign-in methods: the
// current password when the user has one, else a two-factor code when
// two-factor authentication is on, else nothing. A stolen session alone
// must not add a sign-in method that outlives the session.
func (h *Handler) confirmIdentity(ctx context.Context, session controlPanelSession, form url.Values) error {
	if session.HasPassword {
		ok, err := h.checkAccountPassword(ctx, session.User.Email, form.Get("current_password"))
		if err != nil {
			return err
		}
		if !ok {
			return errIdentityPassword
		}
		return nil
	}
	row, found, err := h.getTOTP(ctx, session.User.ID)
	if err != nil {
		return err
	}
	if !found || !row.ConfirmedAt.Valid {
		return nil
	}
	_, err = h.verifyTwoFactorCode(ctx, session.User.ID, form.Get("code"), true)
	return err
}

// signInMethodChange runs the checks that link and unlink share. It returns
// false when it has written the response.
func (h *Handler) signInMethodChange(w http.ResponseWriter, r *http.Request) (controlPanelSession, string, bool) {
	session, _ := sessionFromContext(r.Context())
	provider := chi.URLParam(r, "provider")
	if !h.sso.Has(provider) {
		http.NotFound(w, r)
		return session, "", false
	}
	if !webutil.ParseForm(w, r) {
		return session, "", false
	}
	err := h.confirmIdentity(r.Context(), session, r.Form)
	if err == nil {
		return session, provider, true
	}
	status, msg := http.StatusUnauthorized, ""
	switch {
	case errors.Is(err, errIdentityPassword):
		msg = "Current password is incorrect"
	case errors.Is(err, errTwoFactorBadCode):
		msg = msgTwoFactorBadCode
	case errors.Is(err, errTwoFactorLocked):
		status, msg = http.StatusTooManyRequests, msgTwoFactorLocked
	case errors.Is(err, errTwoFactorUnavailable):
		status, msg = http.StatusServiceUnavailable, msgTwoFactorBroken
	default:
		slog.ErrorContext(r.Context(), "control panel sso identity check", "err", err)
		http.Error(w, "identity check failed", http.StatusInternalServerError)
		return session, "", false
	}
	h.renderSignInMethodError(w, r, session, status, msg)
	return session, "", false
}

func (h *Handler) renderSignInMethodError(w http.ResponseWriter, r *http.Request, session controlPanelSession, status int, msg string) {
	h.renderAccount(w, r, session, status, func(vm *AccountView) {
		vm.FieldErrors = map[string]string{"sign_in_methods": msg}
	})
}

func (h *Handler) ssoLinkStart(w http.ResponseWriter, r *http.Request) {
	session, _, ok := h.signInMethodChange(w, r)
	if !ok {
		return
	}
	h.startSSO(w, r, sso.ModeLink, strconv.FormatInt(session.User.ID, 10), "")
}

func (h *Handler) ssoCallback(w http.ResponseWriter, r *http.Request) {
	pending, id, err := h.sso.Finish(w, r, chi.URLParam(r, "provider"))
	switch {
	case errors.Is(err, sso.ErrUnknownProvider):
		http.NotFound(w, r)
		return
	case errors.Is(err, sso.ErrBadState):
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginInvalid)
		redirectWithNotice(w, r, pathControlPanelLogin, noticeState, nil)
		return
	case errors.Is(err, sso.ErrDenied):
		h.redirectSSOOrigin(w, r, pending, noticeDenied)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "control panel sso callback", "provider", pending.Provider, "err", err)
		h.redirectSSOOrigin(w, r, pending, noticeProvider)
		return
	}
	switch pending.Mode {
	case sso.ModeSignIn:
		h.ssoSignIn(w, r, id)
	case sso.ModeLink:
		h.ssoLink(w, r, pending, id)
	case sso.ModeInvite:
		h.ssoAcceptInvite(w, r, pending, id)
	case sso.ModeSignupAccept:
		h.ssoAcceptSignup(w, r, pending, id)
	default:
		redirectWithNotice(w, r, pathControlPanelLogin, noticeState, nil)
	}
}

// redirectSSOOrigin sends the person back to the page the flow started
// from, with a notice.
func (h *Handler) redirectSSOOrigin(w http.ResponseWriter, r *http.Request, pending sso.Pending, notice string) {
	switch pending.Mode {
	case sso.ModeLink:
		redirectWithNotice(w, r, pathControlPanelAccount, notice, nil)
	case sso.ModeInvite:
		redirectWithNotice(w, r, pathInviteAccept, notice, url.Values{"code": {pending.Code}})
	case sso.ModeSignupAccept:
		redirectWithNotice(w, r, pathSignupAccept, notice, url.Values{"code": {pending.Code}})
	default:
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginInvalid)
		redirectWithNotice(w, r, pathControlPanelLogin, notice, nil)
	}
}

func (h *Handler) ssoSignIn(w http.ResponseWriter, r *http.Request, id sso.Identity) {
	var row sqlcgen.GetControlPanelUserByConnectionRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var qerr error
		row, qerr = q.GetControlPanelUserByConnection(r.Context(), sqlcgen.GetControlPanelUserByConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		})
		if qerr != nil || row.DisabledAt.Valid {
			return qerr
		}
		if qerr := q.TouchControlPanelUserConnection(r.Context(), sqlcgen.TouchControlPanelUserConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		}); qerr != nil {
			return qerr
		}
		// The failure counter guards password guesses. A provider sign-in
		// proves the owner is here, so it clears the counter the same way
		// a correct password does.
		return q.RecordControlPanelLoginSuccess(r.Context(), row.ID)
	})
	// A disabled user gets the same answer as an unknown connection, as the
	// password login does for a disabled user and an unknown email.
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.DisabledAt.Valid) {
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginInvalid)
		redirectWithNotice(w, r, pathControlPanelLogin, noticeNoAccount, nil)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "control panel sso sign-in", "err", err)
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	user := controlPanelUser{ID: row.ID, Email: row.Email, IsPlatformAdmin: row.IsPlatformAdmin}
	if !row.EmailVerifiedAt.Valid {
		h.requireVerification(w, r, user)
		return
	}
	h.finishLoginVia(w, r, user, id.Provider)
}

// ssoAcceptUser is the user an emailed invite or signup approval resolves to
// when the person accepts with a provider identity.
type ssoAcceptUser struct {
	ID              int64
	Email           string
	IsPlatformAdmin bool
	IsNew           bool
}

// resolveSSOAcceptUser finds or creates the user for the invited email. The
// emailed code proves the email, so a new user gets the invited email and
// the provider email is not used. A user that exists is accepted only when
// the identity is linked to that user already.
func (h *Handler) resolveSSOAcceptUser(ctx context.Context, tx pgx.Tx, email string, platformAdmin bool, id sso.Identity) (ssoAcceptUser, error) {
	q := sqlcgen.New(tx)
	conn := sqlcgen.GetControlPanelUserByConnectionParams{Provider: id.Provider, Subject: id.Subject}
	holder, herr := q.GetControlPanelUserByConnection(ctx, conn)
	if herr != nil && !errors.Is(herr, pgx.ErrNoRows) {
		return ssoAcceptUser{}, herr
	}
	linked := herr == nil

	user, gerr := q.GetControlPanelUserAnyStatusByEmail(ctx, email)
	switch {
	case errors.Is(gerr, pgx.ErrNoRows) && linked:
		return ssoAcceptUser{}, errSSOSubjectTaken
	case errors.Is(gerr, pgx.ErrNoRows):
		return h.createSSOUser(ctx, tx, email, platformAdmin, id)
	case gerr != nil:
		return ssoAcceptUser{}, gerr
	case user.DisabledAt.Valid:
		return ssoAcceptUser{}, errInviteForDisabledAccount
	case !linked || holder.ID != user.ID:
		return ssoAcceptUser{}, errSSOAcceptNotLinked
	}
	if err := q.TouchControlPanelUserConnection(ctx, sqlcgen.TouchControlPanelUserConnectionParams(conn)); err != nil {
		return ssoAcceptUser{}, err
	}
	return ssoAcceptUser{ID: user.ID, Email: user.Email, IsPlatformAdmin: user.IsPlatformAdmin}, nil
}

func (h *Handler) createSSOUser(ctx context.Context, tx pgx.Tx, email string, platformAdmin bool, id sso.Identity) (ssoAcceptUser, error) {
	q := sqlcgen.New(tx)
	created, err := q.CreateVerifiedControlPanelUser(ctx, sqlcgen.CreateVerifiedControlPanelUserParams{
		Email:           email,
		IsPlatformAdmin: platformAdmin,
	})
	if err != nil {
		return ssoAcceptUser{}, fmt.Errorf("sso create user: %w", err)
	}
	if err := q.InsertControlPanelUserConnection(ctx, sqlcgen.InsertControlPanelUserConnectionParams{
		ControlPanelUserID: created.ID, Provider: id.Provider, Subject: id.Subject,
	}); err != nil {
		return ssoAcceptUser{}, fmt.Errorf("sso create connection: %w", err)
	}
	if err := auditlog.WritePlatform(ctx, tx, created.ID, "control_panel.sso_link", created.Email,
		map[string]string{"provider": id.Provider}); err != nil {
		return ssoAcceptUser{}, err
	}
	return ssoAcceptUser{ID: created.ID, Email: created.Email, IsPlatformAdmin: created.IsPlatformAdmin, IsNew: true}, nil
}

// acceptNotice maps an accept failure to the notice for the accept page.
// The page itself reports a dead or disabled invite, so those need none.
func acceptNotice(err error) string {
	switch {
	case errors.Is(err, errSSOAcceptNotLinked):
		return noticeAcceptNoLink
	case errors.Is(err, errSSOSubjectTaken):
		return noticeSubjectTaken
	case errors.Is(err, errSignupNameTaken):
		return noticeNameTaken
	case errors.Is(err, errInviteExpired), errors.Is(err, errInviteNotFound), errors.Is(err, errInviteForDisabledAccount):
		return ""
	default:
		return noticeFailed
	}
}

func (h *Handler) redirectAcceptFailure(w http.ResponseWriter, r *http.Request, path, code string, err error) {
	notice := acceptNotice(err)
	if notice == noticeFailed {
		slog.ErrorContext(r.Context(), "control panel sso accept", "err", err)
	}
	query := url.Values{"code": {code}}
	if notice != "" {
		query.Set(ssoNoticeParam, notice)
	}
	http.Redirect(w, r, path+"?"+query.Encode(), http.StatusSeeOther)
}

func (h *Handler) ssoAcceptInvite(w http.ResponseWriter, r *http.Request, pending sso.Pending, id sso.Identity) {
	res, err := h.acceptInvite(r.Context(), acceptInviteInput{Code: pending.Code, Identity: &id})
	if err != nil {
		h.redirectAcceptFailure(w, r, pathInviteAccept, pending.Code, err)
		return
	}
	if res.IsNewUser {
		h.metrics.Signup(observability.SignupControlPanelUser)
	}
	h.finishLoginVia(w, r, controlPanelUser{ID: res.UserID, Email: res.Email}, id.Provider)
}

func (h *Handler) ssoAcceptSignup(w http.ResponseWriter, r *http.Request, pending sso.Pending, id sso.Identity) {
	res, err := h.acceptTenantSignup(r.Context(), signupAcceptInput{Code: pending.Code, Identity: &id})
	if err != nil {
		h.redirectAcceptFailure(w, r, pathSignupAccept, pending.Code, err)
		return
	}
	if res.IsNewUser {
		h.metrics.Signup(observability.SignupControlPanelUser)
	}
	h.finishLoginVia(w, r, controlPanelUser{ID: res.UserID, Email: res.Email}, id.Provider)
}

func (h *Handler) ssoLink(w http.ResponseWriter, r *http.Request, pending sso.Pending, id sso.Identity) {
	session, ok := h.sessionFromRequest(r)
	if !ok || strconv.FormatInt(session.User.ID, 10) != pending.Actor {
		redirectWithNotice(w, r, pathControlPanelLogin, noticeLinkSession, nil)
		return
	}
	notice := noticeLinked
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		holder, qerr := q.GetControlPanelUserByConnection(r.Context(), sqlcgen.GetControlPanelUserByConnectionParams{
			Provider: id.Provider, Subject: id.Subject,
		})
		switch {
		case qerr == nil && holder.ID == session.User.ID:
			notice = noticeAlreadyLinked
			return nil
		case qerr == nil:
			// The rejection commits with its audit row, so the operator
			// can find the two users. The browser learns nothing about
			// the other user.
			notice = noticeSubjectTaken
			return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.sso_link_collision", session.User.Email,
				map[string]any{"provider": id.Provider, "linked_user_id": holder.ID})
		case !errors.Is(qerr, pgx.ErrNoRows):
			return qerr
		}
		connections, qerr := q.ListControlPanelUserConnections(r.Context(), session.User.ID)
		if qerr != nil {
			return qerr
		}
		for _, c := range connections {
			if c.Provider == id.Provider {
				notice = noticeProviderTaken
				return nil
			}
		}
		if qerr := q.InsertControlPanelUserConnection(r.Context(), sqlcgen.InsertControlPanelUserConnectionParams{
			ControlPanelUserID: session.User.ID, Provider: id.Provider, Subject: id.Subject,
		}); qerr != nil {
			return qerr
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.sso_link", session.User.Email,
			map[string]string{"provider": id.Provider})
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "control panel sso link", "err", err)
		notice = noticeFailed
	}
	redirectWithNotice(w, r, pathControlPanelAccount, notice, nil)
}

func (h *Handler) ssoUnlink(w http.ResponseWriter, r *http.Request) {
	session, provider, ok := h.signInMethodChange(w, r)
	if !ok {
		return
	}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// The row lock makes two unlink requests run one after the other,
		// so they cannot both remove what each sees as a spare method.
		hasPassword, qerr := q.LockControlPanelUserSignInMethods(r.Context(), session.User.ID)
		if qerr != nil {
			return qerr
		}
		removed, qerr := q.DeleteControlPanelUserConnection(r.Context(), sqlcgen.DeleteControlPanelUserConnectionParams{
			ControlPanelUserID: session.User.ID, Provider: provider,
		})
		if qerr != nil {
			return qerr
		}
		if removed == 0 {
			return errSSONotLinked
		}
		remaining, qerr := q.CountControlPanelUserConnections(r.Context(), session.User.ID)
		if qerr != nil {
			return qerr
		}
		if !hasPassword && remaining == 0 {
			return errSSOLastMethod
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.sso_unlink", session.User.Email,
			map[string]string{"provider": provider})
	})
	label := h.sso.Label(provider)
	switch {
	case errors.Is(err, errSSONotLinked):
		h.renderSignInMethodError(w, r, session, http.StatusConflict, label+" is not linked.")
	case errors.Is(err, errSSOLastMethod):
		h.renderSignInMethodError(w, r, session, http.StatusConflict,
			label+" is your only sign-in method. Set a password or link a different provider first.")
	case err != nil:
		slog.ErrorContext(r.Context(), "control panel sso unlink", "err", err)
		http.Error(w, "unlink failed", http.StatusInternalServerError)
	default:
		h.renderAccount(w, r, session, http.StatusOK, func(vm *AccountView) {
			vm.Message = label + " unlinked."
		})
	}
}
