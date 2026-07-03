package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/rbac"
)

func relayCredentialsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		endUserID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}
		projectID, ok := enduser.ProjectIDFromContext(ctx)
		if !ok {
			projectID, ok = db.ProjectFromContext(ctx)
		}
		if !ok {
			http.Error(w, "no project", http.StatusForbidden)
			return
		}
		if d.RBAC == nil {
			http.Error(w, "authorization unavailable", http.StatusInternalServerError)
			return
		}
		allowed, err := d.RBAC.CanEndUser(tenantID, endUserID, rbac.ProjectRelayObject(projectID), rbac.ActionIssueCredentials)
		if err != nil {
			http.Error(w, "authorization check failed", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		enabled, err := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureP2PRelay)
		if err != nil {
			http.Error(w, "feature check failed", http.StatusInternalServerError)
			return
		}
		if !enabled {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Tenant-ban enforcement point: a banned account can't get relay
		// credentials.
		if banned, berr := endUserTenantBanned(ctx, d, endUserID); berr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		} else if banned {
			http.Error(w, "account banned", http.StatusForbidden)
			return
		}
		creds, err := d.RelayIssuer.Issue(tenantID, endUserID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The password field is the TURN-REST HMAC, intentionally returned
		// to the authenticated end-user so they can authenticate against
		// the relay. Not a secret-at-rest leak.
		_ = json.NewEncoder(w).Encode(creds) //nolint:gosec // G117: TURN-REST credential payload
	}
}
