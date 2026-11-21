package controlpanel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/storagelimit"
	"github.com/automoto/gg-scale/internal/webutil"
)

// storagePlatformDefault is the platform storage cap from config, or the
// compiled fallback when unset.
func (h *Handler) storagePlatformDefault() int64 {
	if h.cfg.StorageMaxValueBytes > 0 {
		return h.cfg.StorageMaxValueBytes
	}
	return storagelimit.DefaultMaxValueBytes
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
	bytes, err := parseStorageMB(r.Form.Get("max_value_mb"))
	if err != nil {
		h.redirectRateLimits(w, r, tenantID, "Storage limit must be a non-negative number (MB).")
		return
	}
	if err := h.setStorageLimit(r.Context(), session.User.ID, tenantID, nil, bytes); err != nil {
		http.Error(w, "storage limit update failed", http.StatusInternalServerError)
		return
	}
	h.redirectRateLimits(w, r, tenantID, "Account Tenant storage limit updated.")
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
	bytes, err := parseStorageMB(r.Form.Get("max_value_mb"))
	if err != nil {
		h.redirectRateLimits(w, r, tenantID, err.Error())
		return
	}
	err = projectadmin.SetStorageLimit(r.Context(), h.pool, h.storageLimits, tenantID, projectID, bytes,
		h.storagePlatformDefault(), projectadmin.Actor{UserID: session.User.ID})
	var limitErr projectadmin.LimitError
	switch {
	case err == nil:
		h.redirectRateLimits(w, r, tenantID, "Game Project storage limit updated.")
	case errors.As(err, &limitErr):
		h.redirectRateLimits(w, r, tenantID, limitErr.Error())
	case errors.Is(err, projectadmin.ErrProjectNotInTenant):
		http.Error(w, "Game Project not found", http.StatusNotFound)
	default:
		http.Error(w, "storage limit update failed", http.StatusInternalServerError)
	}
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

// parseStorageMB parses a megabyte (MiB) value and returns the equivalent byte
// count; blank or "0" clears the override.
func parseStorageMB(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mb, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, projectadmin.ErrInvalidStorageMB
	}
	return projectadmin.StorageBytesFromMB(mb)
}
