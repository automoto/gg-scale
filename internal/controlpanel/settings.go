package controlpanel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/billing"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/webutil"
)

// tenantSettingsPage consolidates tenant-scoped configuration (API rate limit,
// tenant facts) on one page. Gated by the tenant group's
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

// tenantSettingsView loads the tenant facts and API override in a single
// bootstrap transaction — the page needs none of the
// per-project rows the rate-limits view assembles.
func (h *Handler) tenantSettingsView(ctx context.Context, tenantID int64) (TenantSettingsView, error) {
	view := TenantSettingsView{TenantID: tenantID, BillingPortalURL: h.cfg.BillingPortalURL}
	view.BillingUpgradeURL, view.BillingUpgradeToken = h.billingLinks(tenantID)
	var currentTier tenant.Tier
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		facts, err := q.GetTenantFacts(ctx, tenantID)
		if err != nil {
			return err
		}
		view.TenantName = facts.Name
		tier := tenant.ClampTier(int(facts.Tier))
		currentTier = tier
		view.Tier = tier.String()
		view.TierClass = int(tier)
		view.QuotasEnforced = facts.EnforceQuotas
		if facts.EnforceQuotas {
			used, err := q.GetTenantStorageUsageByID(ctx, tenantID)
			if err != nil {
				return err
			}
			limit := quota.LimitsForClass(tier).StorageBytes
			view.StorageUsedBytes = used
			view.StorageLimitBytes = limit
			view.StorageUsedLabel = formatBytes(used)
			view.StorageLimitLabel = formatBytes(limit)
			if limit > 0 {
				view.StoragePercent = int(used * 100 / limit)
				view.StorageWarn = used*100 >= limit*80
			}
		}
		return nil
	})
	if err != nil {
		return TenantSettingsView{}, err
	}
	if err := h.loadChangeRequestSection(ctx, tenantID, currentTier, &view); err != nil {
		return TenantSettingsView{}, err
	}
	return view, nil
}

// billingLinks returns the external upgrade URL plus a freshly minted handoff
// token for it. Both empty unless the upgrade URL is configured and a handoff
// key was loaded — the link only renders when the billing service can
// actually verify the token.
func (h *Handler) billingLinks(tenantID int64) (upgradeURL, token string) {
	if h.cfg.BillingUpgradeURL == "" || len(h.billingHandoffKey) == 0 {
		return "", ""
	}
	return h.cfg.BillingUpgradeURL, billing.SignHandoff(h.billingHandoffKey, tenantID, billing.DefaultHandoffTTL, h.now())
}

var errInvalidTenantTier = errors.New("control panel: invalid tenant tier")

// updateTenantTierHandler applies a direct platform-admin tier change. Tenant
// upgrade requests remain separately constrained to upward-only transitions.
func (h *Handler) updateTenantTierHandler(w http.ResponseWriter, r *http.Request) {
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
	target, err := parseRequestedTier(r.Form.Get("tier"))
	if err != nil {
		h.redirectTenantSettings(w, r, tenantID, "Choose a valid tenant tier.")
		return
	}
	changed, err := h.setTenantTier(r.Context(), session.User.ID, tenantID, target)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "tenant tier: update", err)
		return
	}
	if !changed {
		h.redirectTenantSettings(w, r, tenantID, "Tenant is already on "+tenant.Tier(target).String()+".")
		return
	}
	h.redirectTenantSettings(w, r, tenantID, "Tenant tier changed to "+tenant.Tier(target).String()+".")
}

func (h *Handler) setTenantTier(ctx context.Context, actorID, tenantID int64, target int16) (bool, error) {
	if target < int16(tenant.Tier0) || target > int16(tenant.Tier3) {
		return false, errInvalidTenantTier
	}
	var changed bool
	tctx := db.WithTenant(ctx, tenantID)
	err := h.pool.Q(tctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).SetTenantTierByID(ctx, sqlcgen.SetTenantTierByIDParams{
			TenantID: tenantID,
			Tier:     target,
		})
		if err != nil {
			return err
		}
		if row.OldTier == row.NewTier {
			return nil
		}
		changed = true
		direction := "upgrade"
		if row.NewTier < row.OldTier {
			direction = "downgrade"
		}
		return auditlog.WritePlatform(tctx, tx, actorID, "control_panel.tenant.tier_change",
			strconv.FormatInt(tenantID, 10), map[string]any{
				"tenant_id": tenantID,
				"old_tier":  row.OldTier,
				"new_tier":  row.NewTier,
				"direction": direction,
			})
	})
	return changed, err
}

// projectSettingsPage consolidates per-project configuration (invite quotas,
// project facts).
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
		TenantID:           tenantID,
		ProjectID:          projectID,
		ProjectName:        proj.Name,
		CreatedAt:          proj.CreatedAt,
		DefaultInviterHour: ratelimit.DefaultInviteLimits.InviterPerHour,
		DefaultDomainDay:   ratelimit.DefaultInviteLimits.DomainPerDay,
	}
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
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

// formatBytes renders a byte count as GB/MB/KB with one decimal for the
// storage-usage display.
func formatBytes(b int64) string {
	const (
		kb = int64(1) << 10
		mb = int64(1) << 20
		gb = int64(1) << 30
	)
	switch {
	case b >= gb:
		return strconv.FormatFloat(float64(b)/float64(gb), 'f', 1, 64) + " GB"
	case b >= mb:
		return strconv.FormatFloat(float64(b)/float64(mb), 'f', 1, 64) + " MB"
	case b >= kb:
		return strconv.FormatFloat(float64(b)/float64(kb), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatInt(b, 10) + " B"
	}
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
