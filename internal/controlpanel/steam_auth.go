package controlpanel

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

// updateSteamAuthHandler saves a project's Steam sign-in credentials. The Web
// API key is write-only: a blank field keeps the stored key, the clear
// checkbox removes it, and no path ever renders it back.
func (h *Handler) updateSteamAuthHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectConfigObject(projectID), rbac.ActionUpdate) {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}

	session, _ := sessionFromContext(r.Context())
	actor := projectadmin.Actor{UserID: session.User.ID}
	if r.Form.Get("steam_clear") != "" {
		h.clearSteamAuth(w, r, tenantID, projectID, actor)
		return
	}

	appID := r.Form.Get("steam_app_id")
	err := projectadmin.SetSteamAuth(r.Context(), h.pool, h.credentialCipher, tenantID, projectID,
		appID, r.Form.Get("steam_web_api_key"), actor)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, projectadmin.ErrInvalidSteamAppID):
		h.renderSteamAuthError(w, r, tenantID, projectID, appID, "steam_app_id", err.Error())
	case errors.Is(err, projectadmin.ErrInvalidSteamKey), errors.Is(err, projectadmin.ErrSteamKeyMissing):
		h.renderSteamAuthError(w, r, tenantID, projectID, appID, "steam_web_api_key", err.Error())
	case err != nil:
		webutil.InternalError(w, "steam auth: update", err)
	default:
		http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
			url.QueryEscape("Steam sign-in settings saved."), http.StatusSeeOther)
	}
}

func (h *Handler) clearSteamAuth(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, actor projectadmin.Actor) {
	err := projectadmin.ClearSteamAuth(r.Context(), h.pool, tenantID, projectID, actor)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "steam auth: clear", err)
		return
	}
	http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
		url.QueryEscape("Steam sign-in disabled."), http.StatusSeeOther)
}

func (h *Handler) renderSteamAuthError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, appID, field, msg string) {
	view, err := h.projectSettingsView(r.Context(), tenantID, projectID)
	if errors.Is(err, errProjectNotInTenant) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "steam auth: load error page", err)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.SteamAppID = appID
	view.FieldErrors = map[string]string{field: msg}
	w.WriteHeader(http.StatusUnprocessableEntity)
	webutil.Render(r, w, ProjectSettingsPage(view))
}
