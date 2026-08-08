package controlpanel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
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
	if err := h.updateRemoteConfig(r.Context(), tenantID, projectID, config); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		webutil.InternalError(w, "remote config: update", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	hash := sha256.Sum256(config)
	if err := h.writePlatformAudit(r.Context(), tenantID, session.User.ID,
		"control_panel.remote_config.update", strconv.FormatInt(projectID, 10), map[string]any{
			"project_id":   projectID,
			"config_bytes": len(config),
			"config_hash":  hex.EncodeToString(hash[:]),
		}); err != nil {
		slog.WarnContext(r.Context(), "audit log: remote config update", "err", err)
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

func (h *Handler) updateRemoteConfig(ctx context.Context, tenantID, projectID int64, config []byte) error {
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).UpdateRemoteConfig(ctx, sqlcgen.UpdateRemoteConfigParams{
			RemoteConfig: config,
			ProjectID:    projectID,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}
