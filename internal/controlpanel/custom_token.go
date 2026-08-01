package controlpanel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/customtoken"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
)

// updateCustomTokenKeyHandler saves the tenant's custom-token verification
// key. The value is a public key: safe to display, nothing to mask. An empty
// submission clears it, which disables custom-token sign-in.
func (h *Handler) updateCustomTokenKeyHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ObjectCustomToken, rbac.ActionManage) {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}

	pemKey := strings.TrimSpace(r.Form.Get("custom_token_public_key"))
	if pemKey != "" {
		if _, err := customtoken.ParsePublicKey(pemKey); err != nil {
			h.renderCustomTokenKeyError(w, r, tenantID, pemKey)
			return
		}
		pemKey += "\n"
	}

	if err := h.updateCustomTokenKey(r.Context(), tenantID, pemKey); errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		webutil.InternalError(w, "custom token key: update", err)
		return
	}

	session, _ := sessionFromContext(r.Context())
	fingerprint := ""
	if pemKey != "" {
		sum := sha256.Sum256([]byte(pemKey))
		fingerprint = hex.EncodeToString(sum[:])
	}
	if err := h.writePlatformAudit(r.Context(), tenantID, session.User.ID,
		"control_panel.custom_token_key.update", strconv.FormatInt(tenantID, 10), map[string]any{
			"tenant_id":       tenantID,
			"cleared":         pemKey == "",
			"key_fingerprint": fingerprint,
		}); err != nil {
		slog.WarnContext(r.Context(), "audit log: custom token key update", "err", err)
	}

	msg := "Custom token signing key saved."
	if pemKey == "" {
		msg = "Custom token sign-in disabled."
	}
	h.redirectTenantSettings(w, r, tenantID, msg)
}

func (h *Handler) renderCustomTokenKeyError(w http.ResponseWriter, r *http.Request, tenantID int64, pemKey string) {
	view, err := h.tenantSettingsView(r.Context(), tenantID)
	if err != nil {
		webutil.InternalError(w, "custom token key: load error page", err)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.IsPlatformAdmin = session.User.IsPlatformAdmin
	view.CustomTokenPublicKey = pemKey
	view.FieldErrors = map[string]string{ //nolint:gosec // form-field name + help text, not credentials
		"custom_token_public_key": "Paste a PEM-encoded Ed25519 or RSA (2048-bit or larger) public key.",
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	webutil.Render(r, w, TenantSettingsPage(view))
}

func (h *Handler) updateCustomTokenKey(ctx context.Context, tenantID int64, pemKey string) error {
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).UpdateTenantCustomTokenPublicKey(ctx, sqlcgen.UpdateTenantCustomTokenPublicKeyParams{
			PublicKey: pemKey,
			TenantID:  tenantID,
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
