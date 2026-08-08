// Package httpapi assembles the ggscale-server HTTP router. The /v1 subtree
// holds all real routes; everything outside falls through to chi's NotFound
// (returning 404). /metrics is mounted at root and is intentionally
// versionless.
package httpapi

import (
	"context"
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

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/cache"
	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/gamesession"
	"github.com/automoto/gg-scale/internal/jobs"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/middleware"
	"github.com/automoto/gg-scale/internal/observability"
	"github.com/automoto/gg-scale/internal/playerauth"
	"github.com/automoto/gg-scale/internal/players"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/relay"
	"github.com/automoto/gg-scale/internal/relaymeter"
	"github.com/automoto/gg-scale/internal/secretseal"
	"github.com/automoto/gg-scale/internal/serverlist"
	"github.com/automoto/gg-scale/internal/steamauth"
	"github.com/automoto/gg-scale/internal/storagelimit"
	"github.com/automoto/gg-scale/internal/tenant"
	"github.com/automoto/gg-scale/internal/twofactor"
	"github.com/automoto/gg-scale/internal/webassets"
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

	// RequestTimeout bounds non-streaming requests; 0 disables the deadline
	// middleware (used by unit-test fixtures). WebSocket paths are exempt.
	RequestTimeout time.Duration

	Pool *db.Pool
	// ReadPool serves staleness-tolerant reads. nil defaults to Pool, so a
	// deployment without a replica (or a test fixture) routes reads to the
	// primary unchanged.
	ReadPool *db.Pool
	Lookup   tenant.Lookup
	Limiter  ratelimit.Limiter
	// RateLimitOverrides (may be nil) supplies per-tenant/project rate-limit
	// overrides. Wrap the DB store in a CachedOverrideStore.
	RateLimitOverrides ratelimit.OverrideStore
	// TokenIPLimits (may be nil) overrides the tier→per-IP bucket derivation
	// for the token auth routes; nil uses ratelimit.TokenIPLimitsForTier.
	TokenIPLimits func(tenant.Tier) ratelimit.Limits
	// StorageMaxValueBytes is the platform default cap on a storage object's
	// value; 0 uses the compiled fallback (1 MiB).
	StorageMaxValueBytes int64
	// StorageLimits (may be nil) resolves per-tenant/project storage-size
	// overrides on top of StorageMaxValueBytes.
	StorageLimits storagelimit.LimitStore
	// ProxyTrust resolves the real client IP for per-IP limits when the server
	// is behind a trusted reverse proxy / load balancer. nil = RemoteAddr only.
	ProxyTrust *ratelimit.ProxyTrust
	Signer     *auth.Signer
	Mailer     mailer.Mailer
	MailFrom   string
	// EnqueuePasswordReset inserts the durable forgot-password delivery job
	// (jobs.PasswordResetEmailArgs). nil = no job queue; the web handlers
	// fall back to in-process delivery.
	EnqueuePasswordReset func(ctx context.Context, surface, email string) error
	// TwoFactor encrypts TOTP secrets and signs 2FA pending cookies for the
	// control panel and player surfaces. nil = 2FA enrollment unavailable.
	TwoFactor *twofactor.Cipher
	// CredentialCipher seals stored provider credentials (e.g. the Steam Web
	// API key) at rest and unseals them on read. nil reads values as stored
	// (unit tests; legacy plaintext rows).
	CredentialCipher *secretseal.Cipher
	// SteamAuth verifies Steam session tickets for POST /v1/auth/steam. nil
	// defaults to the production Valve client; tests inject a fake.
	SteamAuth steamauth.Verifier
	// EmailVerifySigningKey signs verification cookies for both web surfaces.
	EmailVerifySigningKey []byte
	Cache                 cache.Store
	Registry              *prometheus.Registry
	// Metrics carries the business/health counters. nil is a no-op (unit tests).
	Metrics *observability.Metrics
	RBAC    *rbac.Authorizer
	Now     func() time.Time

	// Fleet is the allocator for game-server slots. nil until a backend is
	// wired. The matchmaker checks for nil and degrades to a not-implemented
	// error when unset.
	Fleet *fleet.Manager

	// Hub fans WS messages out to connected players. nil disables /v1/ws.
	Hub                  *realtime.Hub
	RealtimeMaxPerTenant int64
	RealtimeMaxPerPlayer int64
	// RealtimeHeartbeat overrides the WS heartbeat/revalidation interval.
	// Zero uses the production default (30s); tests shorten it.
	RealtimeHeartbeat time.Duration
	// TenantConnectionCap coordinates regional capacity through PostgreSQL
	// leases while keeping socket admission in process memory.
	TenantConnectionCap ratelimit.ConnectionCap

	// Matchmaker is the ticket queue. nil disables /v1/matchmaker/*.
	Matchmaker matchmaker.Queue
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
	// RelayMeter enforces the per-class monthly relay-session allowance at
	// credential issuance. nil disables metering (unit tests).
	RelayMeter *relaymeter.Meter

	ControlPanel          controlpanel.Config
	ControlPanelBootstrap *controlpanel.Bootstrap
	// BillingHandoffKey signs the control panel's short-lived billing upgrade
	// handoff tokens. nil = no upgrade link renders.
	BillingHandoffKey []byte
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

	// EntitlementAPIToken, when non-empty, mounts the internal declarative
	// entitlement API at /internal/entitlements behind this bearer token —
	// outside /v1, so it never enters openapi.yaml or the SDKs. Empty (the
	// default) leaves the surface unmounted entirely.
	EntitlementAPIToken string
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
	if d.SteamAuth == nil {
		d.SteamAuth = steamauth.New()
	}
	// No replica configured (or a test fixture): read-heavy handlers fall back
	// to the primary, so behavior is identical to a single-pool deployment.
	if d.ReadPool == nil {
		d.ReadPool = d.Pool
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
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Session-Token", "X-Request-Id", "If-Match", "If-None-Match"},
		ExposedHeaders:   []string{"X-Request-Id", "X-API-Version", "Retry-After", "ETag"},
		AllowCredentials: false,
		MaxAge:           300,
	}))
	metricsHandler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
	if d.MetricsAuthToken != "" {
		metricsHandler = requireMetricsToken(d.MetricsAuthToken, metricsHandler)
	}
	r.Handle("/metrics", metricsHandler)
	r.Get("/favicon.ico", webassets.FaviconHandler())
	mountInternalAPI(r, d)

	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.NewRequestID())
		r.Use(middleware.NewVersion(d.Version, reg))
		r.Use(middleware.NewObservability(reg))
		if d.RequestTimeout > 0 {
			r.Use(middleware.NewRequestDeadline(d.RequestTimeout))
		}
		registerHealthz(groupAPI(r, humaCfg), d)
		// Shared front-end assets (Pico, stylesheet, fonts) for both the
		// control panel and player surfaces. Mounted unconditionally so player
		// pages stay styled even when the control panel is disabled.
		r.Mount("/assets", webassets.Handler())
		if d.ControlPanel.Enabled() {
			r.Mount("/control-panel", controlpanel.New(controlpanel.Deps{
				Pool:                 d.Pool,
				Cache:                d.Cache,
				Limiter:              d.Limiter,
				RateLimitOverrides:   d.RateLimitOverrides,
				ProxyTrust:           d.ProxyTrust,
				Registry:             reg,
				Metrics:              d.Metrics,
				Config:               d.ControlPanel,
				Bootstrap:            d.ControlPanelBootstrap,
				Mailer:               d.Mailer,
				Fleet:                d.Fleet,
				RBAC:                 d.RBAC,
				PluginInfo:           d.ControlPanelPluginInfo,
				TwoFactor:            d.TwoFactor,
				CredentialCipher:     d.CredentialCipher,
				VerifySigningKey:     d.EmailVerifySigningKey,
				StorageLimits:        d.StorageLimits,
				BillingHandoffKey:    d.BillingHandoffKey,
				EnqueuePasswordReset: passwordResetEnqueuer(d, jobs.PasswordResetSurfaceControlPanel),
			}))
		}
		if d.Players.Enabled() && d.Pool != nil {
			r.Mount("/players", players.New(players.Deps{
				Pool:                 d.Pool,
				Mailer:               d.Mailer,
				MailFrom:             d.MailFrom,
				Config:               d.Players,
				Limiter:              d.Limiter,
				ProxyTrust:           d.ProxyTrust,
				Registry:             reg,
				Metrics:              d.Metrics,
				TwoFactor:            d.TwoFactor,
				VerifySigningKey:     d.EmailVerifySigningKey,
				EnqueuePasswordReset: passwordResetEnqueuer(d, jobs.PasswordResetSurfacePlayerAccount),
			}))
		}
		// One-click invite-email unsubscribe. Mounted independently of the
		// player site so the link in an invite email always resolves. Behind
		// the signed-out per-IP cap like every other public surface: the
		// signed token gates WHO can suppress an address, the limiter bounds
		// how often a valid token can be replayed into database writes.
		if d.Pool != nil && len(d.EmailVerifySigningKey) > 0 {
			r.Group(func(r chi.Router) {
				if d.Limiter != nil {
					r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, reg))
				}
				r.Mount("/unsubscribe", players.NewUnsubscribeHandler(d.Pool, d.EmailVerifySigningKey))
			})
		}

		if d.hasAuthDeps() {
			r.Group(func(r chi.Router) {
				r.Use(tenant.New(d.Lookup))
				r.Use(ratelimit.New(d.Limiter, d.RateLimitOverrides, reg))
				registerRemoteConfig(groupAPI(r, humaCfg), d)

				// /v1/auth/* — tenant-scoped, player-anonymous (api_key
				// suffices). signup/login keep the fixed per-IP cap:
				// bcrypt (~250ms CPU per attempt) is an absolute cost no
				// tier should scale. The token routes swap that cap
				// (which bound them ~6000× below advertised tier rates)
				// for a tier-scaled per-(tenant, IP) cap on publishable
				// keys — see ratelimit.NewTokenIPLimiter for the threat
				// model and the secret-key exemption.
				r.Group(func(r chi.Router) {
					r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, reg))
					registerAuthPasswordRoutes(groupAPI(r, humaCfg), d)
				})
				r.Group(func(r chi.Router) {
					r.Use(ratelimit.NewTokenIPLimiter(d.Limiter, d.TokenIPLimits, d.ProxyTrust, reg))
					registerAuthTokenRoutes(groupAPI(r, humaCfg), d)
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
					// Server-tier player actions: score submission and storage
					// for a player named by id. RBAC on top of the key-type
					// gate, mirroring the player-session submit route.
					r.Group(func(r chi.Router) {
						r.Use(requireAPIKeyPermission(d, rbac.ObjectLeaderboard, rbac.ActionSubmit))
						registerServerLeaderboardSubmit(groupAPI(r, humaCfg), d)
					})
					r.Group(func(r chi.Router) {
						r.Use(requireAPIKeyPermission(d, rbac.ObjectStorage, rbac.ActionManage))
						registerServerStorageRoutes(groupAPI(r, humaCfg), d)
					})
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
					registerAuthLinkRoutes(papi, d)
					registerAuthAccountRoutes(papi, d)
					registerPresence(papi, d)
					registerGameInvites(papi, d)
					registerProfileRoutes(papi, d)
					registerPlayerLookupRoutes(papi, d)
					registerFriendCodeRoutes(papi, d)
					registerStorageRoutes(papi, d)
					registerLeaderboardReadRoutes(papi, d)
					registerLeaderboardDiscoveryRoutes(papi, d)
					registerLeaderboardPeriodRoutes(papi, d)
					registerFriendRoutes(papi, d)
					registerRemoteAddrRoutes(papi, d)
					registerGameSessionRoutes(papi, d)

					// Score submission authorizes in the handler, not here:
					// boards are server-authoritative (secret key) by
					// default, but a board may opt in to publishable-key
					// client submissions, and that flag is per-board data
					// the router cannot see. The player session in
					// X-Session-Token still identifies the subject.
					registerLeaderboardSubmit(papi, d)
				})
			})
		}
	})

	return r
}
