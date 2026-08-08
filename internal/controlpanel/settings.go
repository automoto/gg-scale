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

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/billing"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
	"github.com/automoto/gg-scale/internal/webutil"
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
		if view.CustomTokenPublicKey, err = q.GetTenantCustomTokenPublicKeyForControlPanel(ctx, tenantID); err != nil {
			return err
		}
		view.TenantName = facts.Name
		view.Disabled = facts.DisabledAt.Valid
		if facts.DisabledBy != nil {
			view.DisabledBy = *facts.DisabledBy
		}
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
			limits, err := resolvedTenantLimits(ctx, q, tenantID, tier)
			if err != nil {
				return err
			}
			limit := limits.StorageBytes
			view.StorageUsedBytes = used
			view.StorageLimitBytes = limit
			view.StorageUsedLabel = formatBytes(used)
			switch limit {
			case quota.Unlimited:
				view.StorageLimitLabel = "unlimited"
			case 0:
				// A zero override is a deliberate hard cap: every growing
				// write is blocked, so the tenant sits at 100%.
				view.StorageLimitLabel = formatBytes(0)
				view.StoragePercent = 100
				view.StorageWarn = true
			default:
				view.StorageLimitLabel = formatBytes(limit)
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

var errInvalidFeature = errors.New("control panel: feature is not directly grantable")

// isGrantableFeature reports whether feature is one a platform admin may grant
// directly — the same umbrella set a tenant can request.
func isGrantableFeature(feature string) bool {
	for _, f := range requestableFeatures {
		if f.Value == feature {
			return true
		}
	}
	return false
}

// updateTenantFeatureHandler applies a direct platform-admin feature grant or
// revoke. Unlike tenant self-service, this skips the change-request queue and
// writes the grant immediately.
func (h *Handler) updateTenantFeatureHandler(w http.ResponseWriter, r *http.Request) {
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
	feature := r.Form.Get("feature")
	if !isGrantableFeature(feature) {
		h.redirectTenantSettings(w, r, tenantID, "Choose a valid feature to grant.")
		return
	}
	enable := r.Form.Get("enabled") == "on"
	if enable && !h.featureEnabledByEnv(feature) {
		h.redirectTenantSettings(w, r, tenantID, "That feature's server switch is off; enable it in server settings first.")
		return
	}
	changed, err := h.setTenantFeatureGrant(r.Context(), session.User.ID, tenantID, feature, enable)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "tenant feature: update", err)
		return
	}
	if changed {
		h.reloadRBACPolicy(r.Context())
	}
	switch {
	case !changed && enable:
		h.redirectTenantSettings(w, r, tenantID, "Feature is already enabled.")
	case !changed:
		h.redirectTenantSettings(w, r, tenantID, "Feature is already disabled.")
	case enable:
		h.redirectTenantSettings(w, r, tenantID, "Feature enabled.")
	default:
		h.redirectTenantSettings(w, r, tenantID, "Feature disabled.")
	}
}

// setTenantFeatureGrant enables or disables a tenant-level feature grant in the
// tenant's RLS context and audits the change. changed is false when the grant
// is already in the requested state.
func (h *Handler) setTenantFeatureGrant(ctx context.Context, actorID, tenantID int64, feature string, enable bool) (bool, error) {
	if !isGrantableFeature(feature) {
		return false, errInvalidFeature
	}
	var changed bool
	tctx := db.WithTenant(ctx, tenantID)
	err := h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		// Reject soft-deleted/absent tenants (GetTenantFacts filters
		// deleted_at IS NULL) so a stale admin page can't re-grant a paid
		// feature to a deleted tenant — matching the tier handler, and making
		// the caller's ErrNoRows→404 branch live.
		if _, err := q.GetTenantFacts(tctx, tenantID); err != nil {
			return err
		}
		if enable {
			held, err := q.ListTenantEnabledFeatures(tctx)
			if err != nil {
				return err
			}
			for _, f := range held {
				if f == feature {
					return nil // already enabled: no-op, no audit
				}
			}
			if err := q.UpsertTenantFeatureGrant(tctx, sqlcgen.UpsertTenantFeatureGrantParams{
				Feature:    feature,
				ApprovedBy: &actorID,
				Reason:     strPtr("platform admin direct grant"),
			}); err != nil {
				return err
			}
		} else {
			n, err := q.DisableTenantFeatureGrant(tctx, sqlcgen.DisableTenantFeatureGrantParams{
				Feature: feature,
				Reason:  strPtr("platform admin direct revoke"),
			})
			if err != nil {
				return err
			}
			if n == 0 {
				return nil // already disabled: no-op, no audit
			}
		}
		changed = true
		return auditlog.WritePlatform(tctx, tx, actorID, "control_panel.tenant.feature_grant",
			strconv.FormatInt(tenantID, 10), map[string]any{
				"tenant_id": tenantID,
				"feature":   feature,
				"enabled":   enable,
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
	// One tenant-scoped transaction for both per-project config reads.
	tctx := db.WithTenant(ctx, tenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		config, qerr := q.GetRemoteConfigForControlPanel(tctx, sqlcgen.GetRemoteConfigForControlPanelParams{
			ProjectID: projectID, TenantID: tenantID,
		})
		if qerr != nil {
			return qerr
		}
		view.RemoteConfig = formatRemoteConfig(config)
		steamCfg, qerr := q.GetProjectSteamAuthConfigForControlPanel(tctx, sqlcgen.GetProjectSteamAuthConfigForControlPanelParams{
			ProjectID: projectID, TenantID: tenantID,
		})
		if qerr != nil {
			return qerr
		}
		view.SteamAppID = steamCfg.SteamAppID
		view.SteamKeyConfigured = steamCfg.SteamKeyConfigured != nil && *steamCfg.SteamKeyConfigured
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSettingsView{}, errProjectNotInTenant
	}
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
