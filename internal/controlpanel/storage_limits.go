package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/storagelimit"
	"github.com/ggscale/ggscale/internal/webutil"
)

// errInvalidStorageBytes is returned when a storage-limit form field is not a
// non-negative integer byte count.
var errInvalidStorageBytes = errors.New("control panel: storage limit must be a non-negative integer (bytes)")

// storagePlatformDefault is the platform storage cap from config, or the
// compiled fallback when unset.
func (h *Handler) storagePlatformDefault() int64 {
	if h.cfg.StorageMaxValueBytes > 0 {
		return h.cfg.StorageMaxValueBytes
	}
	return storagelimit.DefaultMaxValueBytes
}

// effectiveTenantStorageLimit returns the tenant-level override if present, else
// the platform default — the ceiling a per-project limit may not exceed.
func (h *Handler) effectiveTenantStorageLimit(ctx context.Context, tenantID int64) (int64, error) {
	def := h.storagePlatformDefault()
	if h.storageLimits == nil {
		return def, nil
	}
	// projectID 0 resolves to the tenant row (or the default) only.
	return h.storageLimits.Resolve(ctx, tenantID, 0, def)
}

// updateTenantStorageLimitHandler sets the tenant-wide storage value cap.
// Platform-admin only, mirroring the tenant API rate limit — a tenant admin
// can't raise their own storage ceiling.
func (h *Handler) updateTenantStorageLimitHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if !session.User.IsPlatformAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	bytes, err := parseStorageBytes(r.Form.Get("max_value_bytes"))
	if err != nil {
		h.redirectRateLimits(w, r, tenantID, "Storage limit must be a non-negative integer (bytes).")
		return
	}
	if err := h.setStorageLimit(r.Context(), session.User.ID, tenantID, nil, bytes); err != nil {
		http.Error(w, "storage limit update failed", http.StatusInternalServerError)
		return
	}
	h.redirectRateLimits(w, r, tenantID, "Tenant storage limit updated.")
}

// updateProjectStorageLimitHandler sets a per-project storage value cap
// (tenant-admin), clamped to the effective tenant limit.
func (h *Handler) updateProjectStorageLimitHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	bytes, err := parseStorageBytes(r.Form.Get("max_value_bytes"))
	if err != nil {
		h.redirectRateLimits(w, r, tenantID, "Storage limit must be a non-negative integer (bytes).")
		return
	}
	if err := h.requireProjectInTenant(r.Context(), tenantID, projectID); err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if bytes > 0 {
		ceiling, cerr := h.effectiveTenantStorageLimit(r.Context(), tenantID)
		if cerr != nil {
			http.Error(w, "storage limit update failed", http.StatusInternalServerError)
			return
		}
		if bytes > ceiling {
			h.redirectRateLimits(w, r, tenantID, "Per-project storage limit can't exceed the tenant limit.")
			return
		}
	}
	pid := projectID
	if err := h.setStorageLimit(r.Context(), session.User.ID, tenantID, &pid, bytes); err != nil {
		http.Error(w, "storage limit update failed", http.StatusInternalServerError)
		return
	}
	h.redirectRateLimits(w, r, tenantID, "Project storage limit updated.")
}

// setStorageLimit persists the override (bytes <= 0 clears it) and writes an
// audit row.
func (h *Handler) setStorageLimit(ctx context.Context, actorID, tenantID int64, projectID *int64, bytes int64) error {
	if h.storageLimits == nil {
		return errors.New("control panel: storage limits unavailable")
	}
	if err := h.storageLimits.Set(ctx, actorID, tenantID, projectID, bytes); err != nil {
		return err
	}
	target := strconv.FormatInt(tenantID, 10)
	payload := map[string]any{"tenant_id": tenantID, "max_value_bytes": bytes}
	if projectID != nil {
		target = strconv.FormatInt(*projectID, 10)
		payload["project_id"] = *projectID
	}
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.storage_limit.set", target, payload)
	})
}

// parseStorageBytes parses a byte count; blank or "0" clears the override.
func parseStorageBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errInvalidStorageBytes
	}
	return n, nil
}
