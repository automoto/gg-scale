package controlpanel

// Tenant disable/enable. A self-disable (tenant admin/owner) blocks the
// tenant's API keys and player traffic but keeps control-panel access so the
// tenant can re-enable or export. A platform disable additionally locks
// tenant admins out of the tenant's pages (enforced in requireTenantAccess);
// only a platform admin can undo it.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	tenantDisabledByTenant   = "tenant"
	tenantDisabledByPlatform = "platform"
)

// tenantPlatformDisabled reports whether the tenant is disabled by the
// platform (a missing/deleted tenant reads as not locked — the page handlers
// 404 it themselves).
func (h *Handler) tenantPlatformDisabled(ctx context.Context, tenantID int64) (bool, error) {
	var state sqlcgen.GetTenantDisabledStateRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		state, qerr = sqlcgen.New(tx).GetTenantDisabledState(ctx, tenantID)
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.DisabledBy != nil && *state.DisabledBy == tenantDisabledByPlatform, nil
}

// canManageTenantLifecycle is the in-handler authz check for disable/enable:
// platform admin, or tenant admin/owner. The control panel is not
// deny-by-default at the router, so the handlers check explicitly.
func (h *Handler) canManageTenantLifecycle(ctx context.Context, session controlPanelSession, tenantID int64) (bool, error) {
	if session.User.IsPlatformAdmin {
		return true, nil
	}
	var role string
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetControlPanelMembership(ctx, sqlcgen.GetControlPanelMembershipParams{
			ControlPanelUserID: session.User.ID,
			TenantID:           tenantID,
		})
		if qerr != nil {
			return qerr
		}
		role = row.Role
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return role == roleAdmin || role == roleOwner, nil
}

func (h *Handler) disableTenantHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	allowed, err := h.canManageTenantLifecycle(r.Context(), session, tenantID)
	if err != nil {
		webutil.InternalError(w, "tenant disable: authz", err)
		return
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	disabledBy := tenantDisabledByTenant
	if session.User.IsPlatformAdmin {
		disabledBy = tenantDisabledByPlatform
	}
	var changed bool
	// tenants RLS only admits UPDATEs under the tenant GUC (the bootstrap
	// policy is SELECT-only), so run in the tenant's scope like the tier
	// handler does.
	tctx := db.WithTenant(r.Context(), tenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var n int64
		var qerr error
		if session.User.IsPlatformAdmin {
			// Supersedes an existing self-disable in place — no brief
			// re-enable window a tenant admin could race.
			n, qerr = q.DisableTenantByPlatformAdmin(tctx, tenantID)
		} else {
			n, qerr = q.DisableTenantBySelf(tctx, tenantID)
		}
		if qerr != nil {
			return qerr
		}
		if n == 0 {
			return nil
		}
		changed = true
		return auditlog.WritePlatform(tctx, tx, session.User.ID, "control_panel.tenant.disable",
			strconv.FormatInt(tenantID, 10), map[string]any{"tenant_id": tenantID, "disabled_by": disabledBy})
	})
	if err != nil {
		webutil.InternalError(w, "tenant disable", err)
		return
	}
	if !changed {
		h.redirectTenantSettings(w, r, tenantID, "Tenant is already disabled.")
		return
	}
	h.redirectTenantSettings(w, r, tenantID, "Tenant disabled. API keys and player traffic are blocked.")
}

func (h *Handler) enableTenantHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	allowed, err := h.canManageTenantLifecycle(r.Context(), session, tenantID)
	if err != nil {
		webutil.InternalError(w, "tenant enable: authz", err)
		return
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var changed bool
	tctx := db.WithTenant(r.Context(), tenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var n int64
		var qerr error
		if session.User.IsPlatformAdmin {
			n, qerr = q.EnableTenantByPlatformAdmin(tctx, tenantID)
		} else {
			// 0 rows for a platform-disabled tenant: only a platform admin
			// can undo a platform disable.
			n, qerr = q.EnableTenantByTenantAdmin(tctx, tenantID)
		}
		if qerr != nil {
			return qerr
		}
		if n == 0 {
			return nil
		}
		changed = true
		return auditlog.WritePlatform(tctx, tx, session.User.ID, "control_panel.tenant.enable",
			strconv.FormatInt(tenantID, 10), map[string]any{"tenant_id": tenantID})
	})
	if err != nil {
		webutil.InternalError(w, "tenant enable", err)
		return
	}
	if !changed {
		h.redirectTenantSettings(w, r, tenantID, "Tenant was not re-enabled. A platform disable can only be undone by a platform admin.")
		return
	}
	h.redirectTenantSettings(w, r, tenantID, "Tenant re-enabled.")
}
