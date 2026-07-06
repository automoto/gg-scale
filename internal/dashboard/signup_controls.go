package dashboard

// Public-join (signup) controls: a tenant master switch plus a per-project
// toggle. Effective per-project join policy = tenant AND project. Both are
// tenant-admin gated at the router (requireTenantAccess(roleAdmin)).

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

// formBool reads an HTML checkbox: present ("on"/"true"/"1") means enabled.
func formBool(r *http.Request, field string) bool {
	switch r.Form.Get(field) {
	case "on", "true", "1":
		return true
	default:
		return false
	}
}

func (h *Handler) setTenantPublicJoiningHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	enabled := formBool(r, "public_joining_enabled")
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		if err := sqlcgen.New(tx).SetTenantPublicJoining(ctx, enabled); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "dashboard.tenant.public_joining",
			strconv.FormatInt(tenantID, 10), map[string]any{"enabled": enabled, "tenant_id": tenantID})
	}); err != nil {
		webutil.InternalError(w, "tenant public joining", err)
		return
	}
	msg := "Tenant public joining enabled."
	if !enabled {
		msg = "Tenant public joining disabled — projects are invite-only."
	}
	base := safeReturnPath(r.Form.Get("redirect_to"), pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+"/projects")
	htmxRedirect(w, r, base+queryFlash+url.QueryEscape(msg))
}

func (h *Handler) setProjectPublicJoiningHandler(w http.ResponseWriter, r *http.Request) {
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
	enabled := formBool(r, "public_joining_enabled")
	session, _ := sessionFromContext(r.Context())
	ctx := db.WithTenant(r.Context(), tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		if err := sqlcgen.New(tx).SetProjectPublicJoining(ctx, sqlcgen.SetProjectPublicJoiningParams{
			ProjectID: projectID,
			Enabled:   enabled,
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, session.User.ID, "dashboard.project.public_joining",
			strconv.FormatInt(projectID, 10), map[string]any{"enabled": enabled, "tenant_id": tenantID, "project_id": projectID})
	}); err != nil {
		webutil.InternalError(w, "project public joining", err)
		return
	}
	msg := "Project public joining enabled."
	if !enabled {
		msg = "Project public joining disabled — invite-only."
	}
	base := safeReturnPath(r.Form.Get("redirect_to"), pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+"/projects")
	htmxRedirect(w, r, base+queryFlash+url.QueryEscape(msg))
}
