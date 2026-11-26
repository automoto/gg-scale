package controlpanel

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/automoto/gg-scale/internal/rbac"
)

func (h *Handler) reloadRBACPolicy(ctx context.Context) {
	if h.rbac == nil {
		return
	}
	if err := h.rbac.ReloadPolicy(); err != nil {
		slog.WarnContext(ctx, "rbac reload after policy update", "err", err)
	}
}

// dropTenantCaches drops what this process caches for a tenant: its API keys
// (they carry the revoked flag, the scopes, the tier, the tenant disabled flag
// and the connection limits), its feature grants, and the management token
// checks (they carry the tenant disabled flag). A write that changes one of
// these calls it after the commit, so the change takes effect at once on this
// process. LISTEN/NOTIFY evicts the key on other instances; cache TTLs and the
// slow fallback cover a lost notification.
func (h *Handler) dropTenantCaches(ctx context.Context, tenantID int64) {
	h.keyCache.InvalidateAndNotify(ctx, tenantID)
	h.rbac.InvalidateFeatureGrants(tenantID)
	h.tokenCache.Invalidate()
}

// requireFleetFeature is route middleware for the dedicated-server fleet
// surface. When the FEATURE_FLEET_ENABLED kill switch is off it returns 404 so
// the section is hidden rather than merely disabled.
func (h *Handler) requireFleetFeature(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.FleetEnabled {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireControlPanelPermission(w http.ResponseWriter, r *http.Request, tenantID int64, obj, act string) bool {
	if h.rbac == nil {
		http.Error(w, "authorization unavailable", http.StatusInternalServerError)
		return false
	}
	session, ok := sessionFromContext(r.Context())
	if !ok {
		http.Error(w, "missing session", http.StatusUnauthorized)
		return false
	}
	allowed, err := h.rbac.CanControlPanel(session.User.ID, tenantID, obj, act)
	if err != nil {
		http.Error(w, "authorization check failed", http.StatusInternalServerError)
		return false
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (h *Handler) requireControlPanelFeature(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, feature rbac.Feature) bool {
	if h.rbac == nil {
		http.Error(w, "authorization unavailable", http.StatusInternalServerError)
		return false
	}
	enabled, err := h.rbac.FeatureEnabled(r.Context(), tenantID, projectID, feature)
	if err != nil {
		http.Error(w, "feature check failed", http.StatusInternalServerError)
		return false
	}
	if !enabled {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// requireProjectRead gates a project-scoped read route on ActionRead for the
// object built from the {projectID} path param. The dedicated-server GET pages
// (fleet config, allocations, matchmaker queue) sit under the coarse
// project:manage route gate, which tenant_admin holds without the finer
// fleet/allocation/matchmaker scopes; this restores the per-object read check
// the write handlers already enforce, so a role can't view infrastructure it
// has no grant to read.
func (h *Handler) requireProjectRead(objectFor func(int64) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, ok := parsePathID(w, r, "tenantID")
			if !ok {
				return
			}
			projectID, ok := parsePathID(w, r, "projectID")
			if !ok {
				return
			}
			if !h.requireControlPanelPermission(w, r, tenantID, objectFor(projectID), rbac.ActionRead) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireControlPanelFleetManage checks the fleet manage right and the
// dedicated_servers feature. It needs no stored data, so the fleet POST
// handlers run it before they load or show a fleet. The backend grant
// (requireControlPanelBackendFeature) follows once the backend is known.
func (h *Handler) requireControlPanelFleetManage(w http.ResponseWriter, r *http.Request, tenantID, projectID int64) bool {
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectFleetObject(projectID), rbac.ActionManage) {
		return false
	}
	return h.requireControlPanelFeature(w, r, tenantID, projectID, rbac.FeatureDedicatedServers)
}

// requireControlPanelBackendFeature checks the feature grant of a fleet
// backend. A backend name that maps to no grant is refused. Allocate runs no
// other name check: Manager.Allocate only refuses a fleet whose backend does
// not match the running one (fleet.BackendMatches).
func (h *Handler) requireControlPanelBackendFeature(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, backend string) bool {
	feature, ok := rbac.BackendFeature(backend)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return h.requireControlPanelFeature(w, r, tenantID, projectID, feature)
}

func (h *Handler) requireControlPanelAllocationMutation(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, action string) bool {
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectAllocationObject(projectID), action) {
		return false
	}
	return h.requireControlPanelFeature(w, r, tenantID, projectID, rbac.FeatureDedicatedServers)
}
