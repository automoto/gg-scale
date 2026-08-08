package controlpanel

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

const steamWebAPIKeyHexLen = 32

var (
	errInvalidSteamAppID = errors.New("control panel: steam app id must be a decimal number")
	errInvalidSteamKey   = errors.New("control panel: steam web api key must be 32 hex characters")
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

	if r.Form.Get("steam_clear") != "" {
		h.clearSteamAuth(w, r, tenantID, projectID)
		return
	}

	appID := r.Form.Get("steam_app_id")
	rawKey := r.Form.Get("steam_web_api_key")
	if err := validateSteamAppID(appID); err != nil {
		h.renderSteamAuthError(w, r, tenantID, projectID, appID,
			"steam_app_id", "Enter the numeric Steam App ID.")
		return
	}
	var key []byte
	if rawKey != "" {
		var err error
		if key, err = validateSteamWebAPIKey(rawKey); err != nil {
			h.renderSteamAuthError(w, r, tenantID, projectID, appID,
				"steam_web_api_key", "Enter the 32-character publisher Web API key.")
			return
		}
	}

	keyChanged, err := h.updateSteamAuth(r.Context(), tenantID, projectID, appID, key)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errSteamKeyMissing) {
		h.renderSteamAuthError(w, r, tenantID, projectID, appID,
			"steam_web_api_key", "Enter your publisher Web API key to enable Steam sign-in.")
		return
	}
	if err != nil {
		webutil.InternalError(w, "steam auth: update", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	if err := h.writePlatformAudit(r.Context(), tenantID, session.User.ID,
		"control_panel.steam_auth.update", strconv.FormatInt(projectID, 10), map[string]any{
			"project_id":  projectID,
			"app_id":      appID,
			"key_changed": keyChanged,
		}); err != nil {
		slog.WarnContext(r.Context(), "audit log: steam auth update", "err", err)
	}

	http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
		url.QueryEscape("Steam sign-in settings saved."), http.StatusSeeOther)
}

func (h *Handler) clearSteamAuth(w http.ResponseWriter, r *http.Request, tenantID, projectID int64) {
	ctx := db.WithTenant(r.Context(), tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ClearProjectSteamAuthConfig(ctx, projectID)
		if err != nil {
			return err
		}
		if rows == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "steam auth: clear", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	if err := h.writePlatformAudit(r.Context(), tenantID, session.User.ID,
		"control_panel.steam_auth.update", strconv.FormatInt(projectID, 10), map[string]any{
			"project_id": projectID,
			"cleared":    true,
		}); err != nil {
		slog.WarnContext(r.Context(), "audit log: steam auth clear", "err", err)
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

// errSteamKeyMissing rejects enabling Steam sign-in with no stored key: the
// form left the key blank (meaning "keep") but nothing was stored yet.
var errSteamKeyMissing = errors.New("control panel: steam key required")

// updateSteamAuth persists the app id and, when key is non-nil, replaces the
// stored Web API key (sealed at rest when the credential cipher is
// configured). Returns whether the key changed.
func (h *Handler) updateSteamAuth(ctx context.Context, tenantID, projectID int64, appID string, key []byte) (bool, error) {
	if key != nil && h.credentialCipher != nil {
		sealed, err := h.credentialCipher.Encrypt(key)
		if err != nil {
			return false, err
		}
		key = sealed
	}
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if key == nil {
			cfg, err := q.GetProjectSteamAuthConfig(ctx, projectID)
			if err != nil {
				return err
			}
			if len(cfg.SteamWebAPIKey) == 0 {
				return errSteamKeyMissing
			}
		}
		rows, err := q.UpdateProjectSteamAuthConfig(ctx, sqlcgen.UpdateProjectSteamAuthConfigParams{
			SteamAppID:     appID,
			SteamWebAPIKey: key,
			ProjectID:      projectID,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	return key != nil, err
}

func validateSteamAppID(s string) error {
	if _, err := strconv.ParseUint(s, 10, 32); err != nil {
		return errInvalidSteamAppID
	}
	return nil
}

// validateSteamWebAPIKey checks the 32-hex-character publisher key format and
// returns the key as the exact bytes Valve expects in the key= parameter.
func validateSteamWebAPIKey(s string) ([]byte, error) {
	if len(s) != steamWebAPIKeyHexLen {
		return nil, errInvalidSteamKey
	}
	if _, err := hex.DecodeString(s); err != nil {
		return nil, errInvalidSteamKey
	}
	return []byte(s), nil
}
