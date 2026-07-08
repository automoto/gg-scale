package controlpanel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/webutil"
)

// tenantSettingsPage consolidates tenant-scoped configuration (public joining,
// API rate limit, tenant facts) on one page. Gated by the tenant group's
// requireTenantAccess(roleAdmin).
func (h *Handler) tenantSettingsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	view, err := h.tenantSettingsView(r.Context(), tenantID)
	if err != nil {
		slog.ErrorContext(r.Context(), "tenant settings load failed", "err", err)
		http.Error(w, "settings load failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.IsPlatformAdmin = session.User.IsPlatformAdmin
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, TenantSettingsPage(view))
}

// tenantSettingsView loads the tenant facts, public-joining switch, and API
// override in a single bootstrap transaction — the page needs none of the
// per-project rows the rate-limits view assembles.
func (h *Handler) tenantSettingsView(ctx context.Context, tenantID int64) (TenantSettingsView, error) {
	view := TenantSettingsView{TenantID: tenantID}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		facts, err := q.GetTenantFacts(ctx, tenantID)
		if err != nil {
			return err
		}
		view.TenantName = facts.Name
		view.Tier = facts.Tier
		view.PublicJoining = facts.PublicJoiningEnabled
		defaults := ratelimit.LimitsForTier(tenant.Tier(facts.Tier))
		view.APIDefaultRate = defaults.RatePerSecond
		view.APIDefaultBurst = defaults.Burst
		rows, err := q.ListAllRateLimitOverridesForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.ProjectID == nil && row.Kind == ratelimit.OverrideKindAPI {
				view.APIOverridden = true
				view.APIRate = row.Rate
				view.APIBurst = row.Burst
			}
		}
		return nil
	})
	if err != nil {
		return TenantSettingsView{}, err
	}
	return view, nil
}

// projectSettingsPage consolidates per-project configuration (public joining,
// invite quotas, project facts).
func (h *Handler) projectSettingsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	view, err := h.projectSettingsView(r.Context(), tenantID, projectID)
	if errors.Is(err, errProjectNotInTenant) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "project settings load failed", "err", err)
		http.Error(w, "settings load failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, ProjectSettingsPage(view))
}

func (h *Handler) projectSettingsView(ctx context.Context, tenantID, projectID int64) (ProjectSettingsView, error) {
	projects, err := h.listProjects(ctx, tenantID)
	if err != nil {
		return ProjectSettingsView{}, err
	}
	proj, found := ProjectOption{}, false
	for _, p := range projects {
		if p.ID == projectID {
			proj, found = p, true
			break
		}
	}
	if !found {
		return ProjectSettingsView{}, errProjectNotInTenant
	}
	view := ProjectSettingsView{
		TenantID:             tenantID,
		ProjectID:            projectID,
		ProjectName:          proj.Name,
		CreatedAt:            proj.CreatedAt,
		ProjectPublicJoining: proj.PublicJoiningEnabled,
		DefaultInviterHour:   ratelimit.DefaultInviteLimits.InviterPerHour,
		DefaultDomainDay:     ratelimit.DefaultInviteLimits.DomainPerDay,
	}
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		facts, err := q.GetTenantFacts(ctx, tenantID)
		if err != nil {
			return err
		}
		view.TenantPublicJoining = facts.PublicJoiningEnabled
		rows, err := q.ListAllRateLimitOverridesForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.ProjectID == nil || *row.ProjectID != projectID {
				continue
			}
			switch row.Kind {
			case ratelimit.OverrideKindInviteInviter:
				view.InviterPerHour = row.Burst
			case ratelimit.OverrideKindInviteDomain:
				view.DomainPerDay = row.Burst
			}
		}
		return nil
	})
	if err != nil {
		return ProjectSettingsView{}, err
	}
	return view, nil
}

// serverSettingsPage renders the read-only, platform-admin-only view of
// server-wide (env) configuration. Gated by the /admin group's
// requirePlatformAdmin.
func (h *Handler) serverSettingsPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, ServerSettingsPage(ServerSettingsView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Snapshot:  h.cfg.ServerSettings,
	}))
}

// safeReturnPath returns raw when it is a same-origin control panel-relative path
// safe to redirect back to after a reused form post, else fallback. It rejects
// absolute URLs, scheme-relative ("//host"), queries/fragments (callers append
// "?flash="), dot segments, and anything outside /v1/control-panel, so the
// server-controlled redirect can't become an open redirect or escape the
// control panel.
func safeReturnPath(raw, fallback string) string {
	if raw == "" {
		return fallback
	}
	if strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n?#") {
		return fallback
	}
	if path.Clean(raw) != raw {
		return fallback
	}
	if raw != pathControlPanel && !strings.HasPrefix(raw, pathControlPanel+"/") {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
		return fallback
	}
	return raw
}
