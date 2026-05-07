// Package httpapi assembles the ggscale-server HTTP router. The /v1 subtree
// holds all real routes; everything outside falls through to chi's NotFound
// (returning 404). /metrics is mounted at root and is intentionally
// versionless.
package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

// Deps carries values the router needs but doesn't construct.
//
// Pool / Lookup / Limiter / Signer are all required to mount the
// tenant-protected /v1/auth/* subgroup. When any is nil, only the public
// /v1/healthz + /metrics routes are mounted — useful for unit tests that
// don't need authenticated paths.
type Deps struct {
	Version string
	Commit  string

	Pool     *db.Pool
	Lookup   tenant.Lookup
	Limiter  ratelimit.Limiter
	Signer   *auth.Signer
	Mailer   mailer.Mailer
	MailFrom string
	Cache    cache.Store
	Registry *prometheus.Registry
	Fleet    *fleet.Registry

	Dashboard          dashboard.Config
	DashboardBootstrap *dashboard.Bootstrap
}

func (d Deps) hasAuthDeps() bool {
	return d.Pool != nil && d.Lookup != nil && d.Limiter != nil && d.Signer != nil
}

// panicRecover catches panics escaping any HTTP handler and turns them
// into 500s instead of letting net/http kill the connection without a
// response. Logs include the stack trace so the operator can locate the
// fault from the access-log slog entry alone.
func panicRecover() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					slog.Error("panic in handler", "panic", fmt.Sprintf("%v", rv), "stack", string(debug.Stack()))
					http.Error(w, "internal error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// NewRouter builds the ggscale-server HTTP handler.
func NewRouter(d Deps) http.Handler {
	reg := d.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}

	r := chi.NewRouter()
	r.Use(panicRecover())
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Session-Token", "X-Request-Id", "If-Match"},
		ExposedHeaders:   []string{"X-Request-Id", "X-API-Version", "Retry-After"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.NewRequestID())
		r.Use(middleware.NewVersion(d.Version, reg))
		r.Use(middleware.NewObservability(reg))
		r.Get("/healthz", healthzHandler(d))
		if d.Dashboard.Enabled() {
			r.Mount("/dashboard", dashboard.New(d.Pool, d.Cache, d.Limiter, reg, d.Dashboard, d.DashboardBootstrap))
		}

		if d.hasAuthDeps() {
			r.Group(func(r chi.Router) {
				r.Use(tenant.New(d.Lookup))
				r.Use(ratelimit.New(d.Limiter, reg))

				// /v1/auth/* — tenant-scoped, end-user-anonymous (api_key
				// suffices). Auth endpoints get an additional per-IP
				// limiter on top of the per-api-key bucket because each
				// login/signup runs bcrypt (~250ms CPU) and a single
				// api_key holder must not be able to burn shared CPU.
				r.Group(func(r chi.Router) {
					r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, reg))
					r.Post("/auth/signup", signupHandler(d))
					r.Post("/auth/verify", verifyHandler(d))
					r.Post("/auth/login", loginHandler(d))
					r.Post("/auth/refresh", refreshHandler(d))
					r.Post("/auth/logout", logoutHandler(d))
					r.Post("/auth/anonymous", anonymousHandler(d))
					r.Post("/auth/custom-token", customTokenHandler(d))
				})

				// Fleet write paths: API-key authenticated only — game
				// servers register with their project-pinned key, no
				// end-user session needed. Secret-only: a publishable
				// key embedded in a shipped game must not be able to
				// register/heartbeat/deregister fleet entries.
				r.Group(func(r chi.Router) {
					r.Use(tenant.RequireKeyType(tenant.KeyTypeSecret))
					mountFleetWriteRoutes(r, d)
				})

				// End-user authenticated: requires X-Session-Token JWT.
				r.Group(func(r chi.Router) {
					r.Use(enduser.New(d.Signer))
					mountStorageRoutes(r, d)
					mountLeaderboardRoutes(r, d)
					mountFriendRoutes(r, d)
					mountProfileRoutes(r, d)
					mountFleetReadRoutes(r, d)
				})
			})
		}
	})

	return r
}
