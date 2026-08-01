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

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
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
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var config map[string]any
	if err := dec.Decode(&config); err != nil || config == nil {
		return nil, errInvalidRemoteConfig
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errInvalidRemoteConfig
	}
	encoded, err := json.Marshal(config)
	if err != nil || len(encoded) > maxRemoteConfigBytes {
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

func (h *Handler) getRemoteConfigForControlPanel(ctx context.Context, tenantID, projectID int64) ([]byte, error) {
	ctx = db.WithTenant(ctx, tenantID)
	var config []byte
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		config, err = sqlcgen.New(tx).GetRemoteConfigForControlPanel(ctx, sqlcgen.GetRemoteConfigForControlPanelParams{
			ProjectID: projectID,
			TenantID:  tenantID,
		})
		return err
	})
	return config, err
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
