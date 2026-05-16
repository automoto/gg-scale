package observability

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// RouteLabel returns the matched chi route pattern (e.g. "/v1/projects/{id}"),
// falling back to "unknown" when chi didn't match a route. Use this anywhere
// the raw r.URL.Path would otherwise become a high-cardinality Prometheus
// label (path segments are attacker-controlled).
func RouteLabel(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unknown"
}
