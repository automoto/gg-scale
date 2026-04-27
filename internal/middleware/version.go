// Package middleware holds HTTP middleware shared across the ggscale-server
// router. Version is the only middleware in Phase 0; rate-limit, tenant, and
// observability middleware land in Phase 1.
package middleware

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
)

// NewVersion returns a middleware that stamps responses with the given API
// version (X-API-Version header) and increments a per-version request counter
// registered with the supplied registry.
func NewVersion(version string, reg prometheus.Registerer) func(http.Handler) http.Handler {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_http_requests_by_version_total",
			Help: "HTTP requests handled, labeled by API version.",
		},
		[]string{"version"},
	)
	reg.MustRegister(counter)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-API-Version", version)
			counter.WithLabelValues(version).Inc()
			next.ServeHTTP(w, r)
		})
	}
}
