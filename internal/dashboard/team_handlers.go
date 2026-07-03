package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	inviteEmailSubjectDashboard = "You've been invited to ggscale"
	inviteEmailSubjectPlatform  = "You've been invited as a ggscale platform admin"
)

func (h *Handler) teamPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	members, pending, err := h.listTenantTeam(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "team list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, TeamPage(TeamView{
		UserEmail:    session.User.Email,
		CSRFToken:    session.CSRFToken,
		TenantID:     tenantID,
		IsOwnerID:    session.User.ID,
		Members:      members,
		Pending:      pending,
		Message:      r.URL.Query().Get("flash"),
		FleetEnabled: h.cfg.FleetEnabled,
	}))

}

func (h *Handler) inviteTeamPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, InviteTeamPage(InviteTeamView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		Role:      roleInviteTenantAdmin,
	}))

}

func (h *Handler) inviteTeammateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	email := r.Form.Get("email")
	role := r.Form.Get("role")

	if retry, throttled := h.inviteThrottled(r.Context(), session.User.ID, tenantID, 0, normalizeEmail(email)); throttled {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		h.renderInviteTeamThrottled(w, r, tenantID, email, role, retry)
		return
	}

	res, err := h.createInvite(r.Context(), inviteTeammateInput{
		Email:     email,
		Role:      role,
		TenantID:  &tenantID,
		InvitedBy: session.User.ID,
	})
	if err != nil {
		// The throttle already debited this send; refund so a failed create
		// (duplicate/invalid/transient) doesn't consume the admin's quota.
		h.inviteRefund(r.Context(), session.User.ID, tenantID, 0, normalizeEmail(email))
		h.renderInviteTeamError(w, r, tenantID, email, role, err)
		return
	}

	h.metrics.InviteSent(observability.InviteTeam)
	h.sendInviteEmail(r.Context(), res, inviteEmailSubjectDashboard, "")
	target := tenantTeamPath(tenantID) + queryFlash + url.QueryEscape("Invite sent to "+res.Email)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) renderInviteTeamError(w http.ResponseWriter, r *http.Request, tenantID int64, email, role string, err error) {
	session, _ := sessionFromContext(r.Context())
	view := InviteTeamView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		Email:     email,
		Role:      role,
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errInvalidInviteEmail):
		status = http.StatusUnprocessableEntity
		view.FieldErrors = map[string]string{"email": "Enter a valid email address"}
	case errors.Is(err, errInvalidInviteRole):
		status = http.StatusUnprocessableEntity
		view.FieldErrors = map[string]string{"role": "Pick a valid role"}
	case errors.Is(err, errInviteExists):
		status = http.StatusConflict
		view.Error = "An invite for that email is already pending. Revoke it before sending a new one."
	default:
		slog.ErrorContext(r.Context(), "dashboard invite create", "err", err)
		view.Error = "Invite could not be sent."
	}
	w.WriteHeader(status)
	webutil.Render(r, w, InviteTeamPage(view))
}

func (h *Handler) renderInviteTeamThrottled(w http.ResponseWriter, r *http.Request, tenantID int64, email, role string, retry int) {
	session, _ := sessionFromContext(r.Context())
	w.WriteHeader(http.StatusTooManyRequests)
	webutil.Render(r, w, InviteTeamPage(InviteTeamView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		Email:     email,
		Role:      role,
		Error:     "Too many invites in a short time. Try again in " + strconv.Itoa(retry) + "s.",
	}))
}

func (h *Handler) revokeInviteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	inviteID, ok := parsePathID(w, r, "inviteID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	if err := h.requireInviteBelongsToTenant(r.Context(), inviteID, tenantID); err != nil {
		http.Error(w, "invite not found", http.StatusNotFound)
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := h.revokeInvite(r.Context(), session.User.ID, inviteID); err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, tenantTeamPath(tenantID)+queryFlash+url.QueryEscape("Invite revoked."))
}

func (h *Handler) updateMemberRoleHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	targetUserID, ok := parsePathID(w, r, "userID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	grant := r.Form.Get("action") == "grant"
	role := r.Form.Get("role")
	session, _ := sessionFromContext(r.Context())
	switch err := h.setTeamMemberRole(r.Context(), session.User.ID, tenantID, targetUserID, role, grant); {
	case err == nil:
		htmxRedirect(w, r, tenantTeamPath(tenantID)+queryFlash+url.QueryEscape("Roles updated."))
	case errors.Is(err, errInvalidGrantRole):
		http.Error(w, "role not grantable", http.StatusForbidden)
	case errors.Is(err, errMemberNotInTenant):
		http.Error(w, "not a member", http.StatusNotFound)
	default:
		http.Error(w, "role update failed", http.StatusInternalServerError)
	}
}

func (h *Handler) removeMemberHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	membershipID, ok := parsePathID(w, r, "membershipID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	err := h.removeMember(r.Context(), session.User.ID, tenantID, membershipID)
	if errors.Is(err, errCannotRemoveSelf) {
		http.Redirect(w, r, tenantTeamPath(tenantID)+queryFlash+url.QueryEscape("You can't remove yourself; ask another admin."), http.StatusSeeOther)
		return
	}
	if err != nil {
		http.Error(w, "remove failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, tenantTeamPath(tenantID)+queryFlash+url.QueryEscape("Member removed."))
}

func (h *Handler) platformTeamPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !session.User.IsPlatformAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	admins, pending, err := h.listPlatformTeam(r.Context())
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, PlatformTeamPage(PlatformTeamView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Admins:    admins,
		Pending:   pending,
		Message:   r.URL.Query().Get("flash"),
	}))

}

func (h *Handler) invitePlatformAdminPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !session.User.IsPlatformAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	webutil.Render(r, w, InvitePlatformAdminPage(InvitePlatformAdminView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
	}))

}

func (h *Handler) invitePlatformAdminHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	if !session.User.IsPlatformAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	email := r.Form.Get("email")
	res, err := h.createInvite(r.Context(), inviteTeammateInput{
		Email:     email,
		Role:      roleInvitePlatformAdmin,
		TenantID:  nil,
		InvitedBy: session.User.ID,
	})
	if err != nil {
		view := InvitePlatformAdminView{
			UserEmail: session.User.Email,
			CSRFToken: session.CSRFToken,
			Email:     email,
		}
		switch {
		case errors.Is(err, errInvalidInviteEmail):
			view.FieldErrors = map[string]string{"email": "Enter a valid email address"}
			w.WriteHeader(http.StatusUnprocessableEntity)
		case errors.Is(err, errInviteExists):
			view.Error = "An invite for that email is already pending."
			w.WriteHeader(http.StatusConflict)
		default:
			slog.ErrorContext(r.Context(), "platform admin invite create", "err", err)
			view.Error = "Invite could not be sent."
			w.WriteHeader(http.StatusInternalServerError)
		}
		webutil.Render(r, w, InvitePlatformAdminPage(view))
		return
	}
	h.sendInviteEmail(r.Context(), res, inviteEmailSubjectPlatform, "")
	http.Redirect(w, r, "/v1/dashboard/admin/team?flash="+url.QueryEscape("Invite sent to "+res.Email), http.StatusSeeOther)
}

func (h *Handler) acceptInvitePage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	res, err := h.lookupInvite(r.Context(), code)
	if err != nil {
		h.renderInviteLookupError(w, r, err)
		return
	}
	webutil.Render(r, w, AcceptInvitePage(AcceptInviteView{
		Code:       code,
		Email:      res.Email,
		Role:       res.Role,
		TenantName: res.TenantName,
		IsPlatform: res.Role == roleInvitePlatformAdmin,
		NewUser:    !res.IsExisting,
		ExpiresAt:  res.ExpiresAt,
		CSRFToken:  webutil.CSRFTokenFromContext(r.Context()),
	}))

}

func (h *Handler) acceptInviteHandler(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	code := r.Form.Get("code")
	password := r.Form.Get("password")

	res, err := h.acceptInvite(r.Context(), acceptInviteInput{Code: code, Password: password})
	if err != nil {
		// Reload the lookup view with an error to keep messaging consistent.
		lookup, lerr := h.lookupInvite(r.Context(), code)
		if lerr != nil {
			h.renderInviteLookupError(w, r, lerr)
			return
		}
		view := AcceptInviteView{
			Code:       code,
			Email:      lookup.Email,
			Role:       lookup.Role,
			TenantName: lookup.TenantName,
			IsPlatform: lookup.Role == roleInvitePlatformAdmin,
			NewUser:    !lookup.IsExisting,
			ExpiresAt:  lookup.ExpiresAt,
			CSRFToken:  webutil.CSRFTokenFromContext(r.Context()),
		}
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errWeakPassword):
			status = http.StatusUnprocessableEntity
			view.FieldErrors = map[string]string{"password": "Password must be at least 12 characters."}
		case errors.Is(err, errInvalidCredentials):
			status = http.StatusUnauthorized
			view.FieldErrors = map[string]string{"password": "Incorrect password for that account."}
		case errors.Is(err, errInviteExpired):
			view.Error = "This invite has expired. Ask the inviter to send a new one."
			status = http.StatusGone
		case errors.Is(err, errInviteNotFound):
			view.Error = "Invite not found or already used."
			status = http.StatusNotFound
		case errors.Is(err, errInviteForDisabledAccount):
			view.Error = "This account has been disabled. Contact your platform admin."
			status = http.StatusForbidden
		default:
			slog.ErrorContext(r.Context(), "accept invite", "err", err)
			view.Error = "Could not accept invite."
		}
		w.WriteHeader(status)
		webutil.Render(r, w, AcceptInvitePage(view))
		return
	}

	if res.IsNewUser {
		h.metrics.Signup(observability.SignupDashboardUser)
	}
	// Auto-login: issue a session and redirect to the dashboard home.
	if _, err := h.issueSession(r.Context(), w, res.UserID, h.clientIP(r), r.Header.Get("User-Agent")); err != nil {
		slog.ErrorContext(r.Context(), "accept invite: issue session", "err", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/v1/dashboard?flash="+url.QueryEscape("Welcome to ggscale, "+res.Email+"!"), http.StatusSeeOther)
}

// requireInviteBelongsToTenant is a small access check: the invite must be
// scoped to the same tenant the actor is currently administering. Platform
// admins bypass via the platform team page; this path only runs for
// tenant-team invite operations.
func (h *Handler) requireInviteBelongsToTenant(ctx context.Context, inviteID, tenantID int64) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetDashboardInvitationByID(ctx, inviteID)
		if err != nil {
			return err
		}
		if row.TenantID == nil || *row.TenantID != tenantID {
			return errInviteNotFound
		}
		return nil
	})
}

func (h *Handler) renderInviteLookupError(w http.ResponseWriter, r *http.Request, err error) {
	var status int
	var msg string
	switch {
	case errors.Is(err, errInviteExpired):
		status = http.StatusGone
		msg = "This invite has expired."
	case errors.Is(err, errInviteNotFound):
		status = http.StatusNotFound
		msg = "Invite not found or already used."
	case errors.Is(err, errInviteForDisabledAccount):
		status = http.StatusForbidden
		msg = "This account has been disabled. Contact your platform admin."
	default:
		status = http.StatusInternalServerError
		msg = "Could not load invite."
	}
	w.WriteHeader(status)
	webutil.Render(r, w, AcceptInvitePage(AcceptInviteView{Error: msg, CSRFToken: webutil.CSRFTokenFromContext(r.Context())}))
}

// sendInviteEmail mails the invite recipient the magic link. Failure is
// logged but does not block the request — the inviter can re-send.
func (h *Handler) sendInviteEmail(ctx context.Context, res inviteResult, subject, extra string) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		slog.WarnContext(ctx, "dashboard invite: no mailer configured", "invite_id", res.ID, "email", res.Email)
		return
	}
	link := h.inviteAcceptURL(res.Code)
	body := strings.TrimSpace(fmt.Sprintf(
		"You were invited to ggscale (%s role).\n\nClick to accept (expires %s):\n%s\n%s",
		res.Role, res.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), link, extra,
	))
	if err := h.mailer.Send(ctx, mailer.Message{
		From:    h.cfg.MailFrom,
		To:      []string{res.Email},
		Subject: subject,
		Body:    body,
	}); err != nil {
		slog.ErrorContext(ctx, "dashboard invite mailer", "err", err, "invite_id", res.ID)
	}
}

func (h *Handler) inviteAcceptURL(code string) string {
	base := strings.TrimRight(h.cfg.BaseURL, "/")
	return base + "/v1/dashboard/invite/accept?code=" + url.QueryEscape(code)
}

func tenantTeamPath(tenantID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) + "/team"
}
