package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/tenant"
)

func requireAPIKeyPermission(d Deps, obj, act string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d.RBAC == nil {
				http.Error(w, "authorization unavailable", http.StatusInternalServerError)
				return
			}
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := d.RBAC.CanAPIKey(key, obj, act)
			if err != nil {
				http.Error(w, "authorization check failed", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mountRealtimeRoutes(r chi.Router, d Deps, reg prometheus.Registerer) {
	if d.Hub == nil {
		return
	}
	// The per-tenant CCU cap needs a shared counter; with no cache it is
	// disabled (per-player still works in-process). At scale run CACHE_BACKEND=
	// olric so the count is global across app hosts rather than per-host.
	var tenantCap ratelimit.ConnectionCap
	if d.Cache != nil {
		tenantCap = ratelimit.NewCacheConnectionCap(d.Cache, reg)
	}
	r.Get("/ws", realtime.ServeWS(realtime.Options{
		Hub:             d.Hub,
		Cache:           d.Cache,
		TenantCap:       tenantCap,
		EnvMaxPerTenant: d.RealtimeMaxPerTenant,
		MaxPerPlayer:    d.RealtimeMaxPerPlayer,
	}))
}
