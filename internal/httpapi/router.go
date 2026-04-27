// Package httpapi assembles the ggscale-server HTTP router. The /v1 subtree
// holds all real routes; everything outside falls through to chi's NotFound
// (returning 404). /metrics is mounted at root and is intentionally
// versionless.
package httpapi

import (
	"net/http"

	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Deps carries values the router needs but doesn't construct: the API version
// stamped on responses and the commit SHA reported by /v1/healthz.
type Deps struct {
	Version string
	Commit  string
}

// NewRouter builds the ggscale-server HTTP handler. /v1 holds all real
// routes; /metrics is mounted at root and is intentionally versionless.
func NewRouter(d Deps) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	r := chi.NewRouter()

	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.NewVersion(d.Version, reg))
		r.Get("/healthz", healthzHandler(d))
	})

	return r
}
