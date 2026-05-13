package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
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
