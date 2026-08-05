package controlpanel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/customtoken"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
)

// updateCustomTokenKeyHandler saves the tenant's custom-token verification
// key. A stored value is always a public key, so the settings page may display
// it; a rejected submission is not, and is never echoed back. An empty
// submission clears the key, which disables custom-token sign-in.
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

// renderCustomTokenKeyError re-renders the settings page with a field error.
// The rejected submission is deliberately not echoed back: a mistakenly pasted
// private key would otherwise sit in the response DOM after an error the user
// may not notice, where a screenshot or a saved page would carry it. The
// textarea falls back to the stored public key that tenantSettingsView loaded.
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
	view.FieldErrors = map[string]string{ //nolint:gosec // form-field name + help text, not credentials
		"custom_token_public_key": customTokenKeyErrorMessage(pemKey),
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	webutil.Render(r, w, TenantSettingsPage(view))
}

// customTokenKeyErrorMessage names the private-key mistake explicitly, because
// the field resets to the stored key and an unexplained reset is confusing.
// The message never quotes the submitted value.
func customTokenKeyErrorMessage(pemKey string) string {
	if block, _ := pem.Decode([]byte(pemKey)); block != nil && strings.Contains(block.Type, "PRIVATE") {
		return "That is a private key. Paste the matching public key; the private key must stay on your backend."
	}
	return "Paste a PEM-encoded Ed25519 or RSA (2048-bit or larger) public key."
}

func (h *Handler) updateCustomTokenKey(ctx context.Context, tenantID int64, pemKey string) error {
	// tenants RLS only admits UPDATEs under the tenant GUC (the bootstrap
	// policy is SELECT-only), so run in the tenant's scope like the tier
	// handler does.
	tctx := db.WithTenant(ctx, tenantID)
	return h.pool.Q(tctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).UpdateTenantCustomTokenPublicKey(tctx, sqlcgen.UpdateTenantCustomTokenPublicKeyParams{
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
