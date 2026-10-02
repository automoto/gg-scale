package controlpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

const maxRemoteConfigBytes = 64 << 10

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
	encoded, err := normalizeJSONObjectBlob(raw, maxRemoteConfigBytes)
	if err != nil {
		return nil, errInvalidRemoteConfig
	}
	return encoded, nil
}

// normalizeJSONObjectBlob validates a single top-level JSON object (no
// trailing data), re-encodes it canonically, and enforces the byte cap on the
// canonical form. Shared by the remote-config editor and the leaderboard
// metadata field.
func normalizeJSONObjectBlob(raw string, maxBytes int) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var blob map[string]any
	if err := dec.Decode(&blob); err != nil || blob == nil {
		return nil, errors.New("control panel: not a JSON object")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("control panel: trailing data after JSON object")
	}
	encoded, err := json.Marshal(blob)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxBytes {
		return nil, errors.New("control panel: JSON object too large")
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
