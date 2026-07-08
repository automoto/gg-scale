// Package httpapi assembles the ggscale-server HTTP router. The /v1 subtree
// holds all real routes; everything outside falls through to chi's NotFound
// (returning 404). /metrics is mounted at root and is intentionally
// versionless.
package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/gamesession"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/players"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/relay"
	"github.com/ggscale/ggscale/internal/serverlist"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/webassets"
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

	Pool    *db.Pool
	Lookup  tenant.Lookup
	Limiter ratelimit.Limiter
	// RateLimitOverrides (may be nil) supplies per-tenant/project rate-limit
	// overrides. Wrap the DB store in a CachedOverrideStore.
	RateLimitOverrides ratelimit.OverrideStore
	// ProxyTrust resolves the real client IP for per-IP limits when the server
	// is behind a trusted reverse proxy / load balancer. nil = RemoteAddr only.
	ProxyTrust *ratelimit.ProxyTrust
	Signer     *auth.Signer
	Mailer     mailer.Mailer
	MailFrom   string
	// TwoFactor encrypts TOTP secrets and signs 2FA pending cookies for the
	// control panel and player surfaces. nil = 2FA enrollment unavailable.
	TwoFactor *twofactor.Cipher
	Cache     cache.Store
	Registry  *prometheus.Registry
	// Metrics carries the business/health counters. nil is a no-op (unit tests).
	Metrics *observability.Metrics
	RBAC    *rbac.Authorizer
	Now     func() time.Time

	// Fleet is the allocator for game-server slots. nil until a backend is
	// wired in M2 (Docker) and onward. The matchmaker (M6) checks for nil
	// and degrades to a not-implemented error when unset.
	Fleet *fleet.Manager

	// Hub fans WS messages out to connected players. nil disables /v1/ws.
	Hub                  *realtime.Hub
	RealtimeMaxPerTenant int64
	RealtimeMaxPerPlayer int64

	// Matchmaker is the ticket queue. nil disables /v1/matchmaker/*.
	Matchmaker matchmaker.Queue
	// MatchmakerMaxTicketsPerPlayer caps a player's concurrently queued
	// tickets per project. 0 disables the cap.
	MatchmakerMaxTicketsPerPlayer int
	// MatchmakerTicketTTL bounds how long a queued ticket lives. 0
	// disables expiry.
	MatchmakerTicketTTL time.Duration

	// GameSessions creates game sessions (shared with the matchmaker
	// worker's game_session mode).
	GameSessions *gamesession.Service

	// ServerList is the in-memory game-server heartbeat registry that
	// backs the server-browser endpoint. nil disables /v1/fleets/*.
	ServerList *serverlist.Registry

	// RelayIssuer mints TURN-REST credentials. nil disables /v1/relay/*.
	RelayIssuer *relay.Issuer

	ControlPanel          controlpanel.Config
	ControlPanelBootstrap *controlpanel.Bootstrap
	// ControlPanelPluginInfo is the closure the admin/plugins page calls to
	// snapshot the running fleet plugin. nil when no plugin backend is
	// configured — the page renders "no plugin backend" in that case.
	ControlPanelPluginInfo func() *controlpanel.PluginSnapshot

	// Players controls whether the player-facing /v1/players/p/{projectID}/
	// site is mounted.
	Players players.Config

	// CORSAllowedOrigins lists the origins the API router answers preflight
	// from. Empty in dev falls back to "*"; config.Validate refuses an
	// empty list in production.
	CORSAllowedOrigins []string

	// MetricsAuthToken, when non-empty, gates /metrics behind a bearer token.
	// Empty leaves /metrics open (dev / explicitly-unauthenticated deployments).
	MetricsAuthToken string
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

// requireMetricsToken gates a handler behind a static bearer token. This is a
// shared-secret guard for Prometheus scraping — deliberately separate from the
// DB-backed tenant API keys (internal/tenant), since a scraper is not a tenant.
//
// Both sides are SHA-256'd before the constant-time compare so the comparison
// always runs over equal-length (32-byte) digests: subtle.ConstantTimeCompare
// short-circuits on a length mismatch, which would otherwise leak the token
// length via timing. Same idiom as tenant key hashing.
func requireMetricsToken(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerCredential(r.Header.Get("Authorization"))
		got := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerCredential extracts the token from an "Authorization: Bearer <token>"
// header. The scheme match is case-insensitive (RFC 7235) and the token is
// whitespace-trimmed — matching the tenant middleware's parser and Prometheus,
// which trims the credentials_file it sends.
func bearerCredential(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// NewRouter builds the ggscale-server HTTP handler.
func NewRouter(d Deps) http.Handler {
	reg := d.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
		reg.MustRegister(collectors.NewGoCollector())
		reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	}
	// The session service is a stateless pool wrapper; default it so
	// callers (and test fixtures) only override when sharing an instance
	// with the matchmaker worker matters.
	if d.GameSessions == nil && d.Pool != nil {
		d.GameSessions = gamesession.NewService(d.Pool)
	}

	// humaCfg carries the single OpenAPI document every migrated /v1 group
	// accumulates operations into (see humaapi.go). Groups bind their own
	// humachi adapter to it via groupAPI.
	humaCfg := newHumaConfig(d.Version)

	r := chi.NewRouter()
	r.Use(panicRecover())
	allowedOrigins := d.CORSAllowedOrigins
	if len(allowedOrigins) == 0 {
		// Dev fallback: wildcard. config.Validate rejects this in prod.
		allowedOrigins = []string{"*"}
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Session-Token", "X-Request-Id", "If-Match"},
		ExposedHeaders:   []string{"X-Request-Id", "X-API-Version", "Retry-After"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	metricsHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
	if d.MetricsAuthToken != "" {
		metricsHandler = requireMetricsToken(d.MetricsAuthToken, metricsHandler)
	}
	r.Handle("/metrics", metricsHandler)

	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.NewRequestID())
		r.Use(middleware.NewVersion(d.Version, reg))
		r.Use(middleware.NewObservability(reg))
		registerHealthz(groupAPI(r, humaCfg), d)
		// Shared front-end assets (Pico, stylesheet, fonts) for both the
		// control panel and player surfaces. Mounted unconditionally so player
		// pages stay styled even when the control panel is disabled.
		r.Mount("/assets", webassets.Handler())
		if d.ControlPanel.Enabled() {
			r.Mount("/control-panel", controlpanel.New(controlpanel.Deps{
				Pool:               d.Pool,
				Cache:              d.Cache,
				Limiter:            d.Limiter,
				RateLimitOverrides: d.RateLimitOverrides,
				ProxyTrust:         d.ProxyTrust,
				Registry:           reg,
				Metrics:            d.Metrics,
				Config:             d.ControlPanel,
				Bootstrap:          d.ControlPanelBootstrap,
				Mailer:             d.Mailer,
				Fleet:              d.Fleet,
				RBAC:               d.RBAC,
				PluginInfo:         d.ControlPanelPluginInfo,
				TwoFactor:          d.TwoFactor,
			}))
		}
		if d.Players.Enabled() && d.Pool != nil {
			r.Mount("/players", players.New(players.Deps{
				Pool:       d.Pool,
				Mailer:     d.Mailer,
				MailFrom:   d.MailFrom,
				Config:     d.Players,
				Limiter:    d.Limiter,
				ProxyTrust: d.ProxyTrust,
				Registry:   reg,
				Metrics:    d.Metrics,
				TwoFactor:  d.TwoFactor,
			}))
		}

		if d.hasAuthDeps() {
			r.Group(func(r chi.Router) {
				r.Use(tenant.New(d.Lookup))
				r.Use(ratelimit.New(d.Limiter, d.RateLimitOverrides, reg))

				// /v1/auth/* — tenant-scoped, player-anonymous (api_key
				// suffices). Auth endpoints get an additional per-IP
				// limiter on top of the per-api-key bucket because each
				// login/signup runs bcrypt (~250ms CPU) and a single
				// api_key holder must not be able to burn shared CPU.
				r.Group(func(r chi.Router) {
					r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, reg))
					registerAuthRoutes(groupAPI(r, humaCfg), d)
				})

				// /v1/server/player-sessions/verify — server-tier endpoint used by
				// game-server workloads to verify a player's session
				// token (the request body) under their own API-key auth
				// (the Authorization header). Gated by RBAC permission so
				// publishable keys (embedded in shipped game binaries) can't
				// be used as a session-validity oracle.
				r.Group(func(r chi.Router) {
					r.Use(requireAPIKeyPermission(d, rbac.ObjectPlayer, rbac.ActionVerify))
					registerPlayerSessionVerify(groupAPI(r, humaCfg), d)
				})

				r.Group(func(r chi.Router) {
					r.Use(tenant.RequireKeyType(tenant.KeyTypeSecret))
					// Server-tier remote-address read: a game server reads a
					// linked player's opaque endpoint for that project. Secret
					// keys only — publishable keys never reach this group.
					registerServerRemoteAddr(groupAPI(r, humaCfg), d)
					if d.ServerList != nil {
						r.Group(func(r chi.Router) {
							r.Use(tenant.RequireKeyScope(tenant.ScopeFleet))
							registerFleetHeartbeat(groupAPI(r, humaCfg), d)
						})
					}
				})

				// Player authenticated: requires X-Session-Token JWT.
				r.Group(func(r chi.Router) {
					r.Use(playerauth.New(d.Signer, epochValidator{d.Pool}))
					r.Use(ratelimit.NewPlayerLimiter(d.Limiter, ratelimit.PlayerRate, ratelimit.PlayerBurst, reg))
					mountRealtimeRoutes(r, d)

					if d.Matchmaker != nil {
						r.Group(func(r chi.Router) {
							r.Use(tenant.RequireKeyScope(tenant.ScopeMatchmaker))
							registerMatchmakerRoutes(groupAPI(r, humaCfg), d)
						})
					}

					if d.ServerList != nil {
						r.Group(func(r chi.Router) {
							r.Use(tenant.RequireKeyScope(tenant.ScopeFleet))
							registerFleetServersList(groupAPI(r, humaCfg), d)
						})
					}
					if d.RelayIssuer != nil {
						r.Group(func(r chi.Router) {
							r.Use(tenant.RequireKeyScope(tenant.ScopeP2PRelay))
							registerRelay(groupAPI(r, humaCfg), d)
						})
					}

					// Typed huma operations. The adapter binds to this
					// same player-authenticated chi group, so
					// tenant/session/rate-limit middleware still runs
					// ahead of every handler.
					papi := groupAPI(r, humaCfg)
					registerPresence(papi, d)
					registerGameInvites(papi, d)
					registerProfileRoutes(papi, d)
					registerStorageRoutes(papi, d)
					registerLeaderboardReadRoutes(papi, d)
					registerFriendRoutes(papi, d)
					registerRemoteAddrRoutes(papi, d)
					registerGameSessionRoutes(papi, d)

					// Score submission is server-authoritative: only
					// callers with a secret key (game server / tenant
					// backend) may submit. The player session in
					// X-Session-Token still identifies the subject.
					r.Group(func(r chi.Router) {
						r.Use(requireAPIKeyPermission(d, rbac.ObjectLeaderboard, rbac.ActionSubmit))
						registerLeaderboardSubmit(groupAPI(r, humaCfg), d)
					})
				})
			})
		}
	})

	return r
}
