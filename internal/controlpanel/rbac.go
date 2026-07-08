package controlpanel

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/ggscale/ggscale/internal/rbac"
)

func (h *Handler) reloadRBACPolicy(ctx context.Context) {
	if h.rbac == nil {
		return
	}
	if err := h.rbac.ReloadPolicy(); err != nil {
		slog.WarnContext(ctx, "rbac reload after policy update", "err", err)
	}
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
	allowed, err := h.rbac.CanControlPanel(rbac.ControlPanelUser{
		ID:              session.User.ID,
		IsPlatformAdmin: session.User.IsPlatformAdmin,
	}, tenantID, obj, act)
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

func (h *Handler) requireControlPanelFleetMutation(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, backend string) bool {
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectFleetObject(projectID), rbac.ActionManage) {
		return false
	}
	if !h.requireControlPanelFeature(w, r, tenantID, projectID, rbac.FeatureDedicatedServers) {
		return false
	}
	if backendFeature, ok := rbac.BackendFeature(backend); ok {
		return h.requireControlPanelFeature(w, r, tenantID, projectID, backendFeature)
	}
	return true
}

func (h *Handler) requireControlPanelAllocationMutation(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, action string) bool {
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectAllocationObject(projectID), action) {
		return false
	}
	return h.requireControlPanelFeature(w, r, tenantID, projectID, rbac.FeatureDedicatedServers)
}
