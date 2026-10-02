package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

var errInvalidRemoteConfig = errors.New("control panel: remote config must be a JSON object up to 64 KiB")

func (h *Handler) updateRemoteConfigHandler(w http.ResponseWriter, r *http.Request) {
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

	raw := r.Form.Get("config")
	config, err := normalizeRemoteConfig(raw)
	if err != nil {
		h.renderRemoteConfigError(w, r, tenantID, projectID, raw)
		return
	}
	session, _ := sessionFromContext(r.Context())
	_, err = projectadmin.SetRemoteConfig(r.Context(), h.pool, tenantID, projectID, config, nil,
		projectadmin.Actor{UserID: session.User.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "remote config: update", err)
		return
	}

	http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
		url.QueryEscape("Remote config saved."), http.StatusSeeOther)
}

func (h *Handler) renderRemoteConfigError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, raw string) {
	view, err := h.projectSettingsView(r.Context(), tenantID, projectID)
	if errors.Is(err, errProjectNotInTenant) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "remote config: load error page", err)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.RemoteConfig = raw
	view.FieldErrors = map[string]string{
		"config": "Enter a JSON object no larger than 64 KiB.",
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	webutil.Render(r, w, ProjectSettingsPage(view))
}

func normalizeRemoteConfig(raw string) ([]byte, error) {
	encoded, err := projectadmin.NormalizeJSONObject(raw, projectadmin.RemoteConfigMaxBytes)
	if err != nil {
		return nil, errInvalidRemoteConfig
	}
	return encoded, nil
}

func formatRemoteConfig(config []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, config, "", "  "); err != nil {
		return string(config)
	}
	return out.String()
}
