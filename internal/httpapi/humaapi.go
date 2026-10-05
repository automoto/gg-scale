package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// v1Prefix is the path every /v1 operation carries in the OpenAPI document.
// The chi groups the humachi adapters bind to are already mounted under this
// prefix (r.Route("/v1", …)), so groupAdapter strips it before registering the
// route while huma keeps the full path in the spec.
const v1Prefix = "/v1"

// playerSecurity is the security requirement for player-tier operations: the
// publishable key AND a player session token. A secret key also works there;
// the PublishableKey scheme description says so.
var playerSecurity = []map[string][]string{{"PublishableKey": {}, "PlayerSession": {}}}

// apiKeySecurity is the requirement for endpoints authenticated by the
// publishable key alone (player-anonymous), e.g. the /v1/auth/* routes.
var apiKeySecurity = []map[string][]string{{"PublishableKey": {}}}

// secretKeySecurity is the requirement for operations that refuse a
// publishable key: the router group behind tenant.RequireKeyType(secret). TestAPIKeyType_spec_matches_router
// (integration) checks the spec against the real router.
var secretKeySecurity = []map[string][]string{{"SecretKey": {}}}

// eitherKeyPlayerSecurity is for score submit: the board decides whether a
// publishable key may submit.
var eitherKeyPlayerSecurity = []map[string][]string{
	{"PublishableKey": {}, "PlayerSession": {}},
	{"SecretKey": {}, "PlayerSession": {}},
}

// newHumaConfig builds the shared OpenAPI config. Every group adapter created
// by groupAPI is constructed from this same value, so its embedded *OpenAPI
// pointer accumulates every registered operation into ONE document — the one
// cmd/openapi-dump emits at cutover.
//
// The DefaultConfig link transformer (which injects a `$schema` field into
// response bodies plus Link headers) is dropped: success payloads must stay
// byte-identical to the frozen wire. The built-in spec/docs/schema routes are
// disabled — the spec is generated offline and no docs UI is served.
func newHumaConfig(version string) huma.Config {
	if version == "" {
		version = "0.0.0"
	}
	cfg := huma.DefaultConfig("ggscale API", version)
	cfg.CreateHooks = nil
	cfg.OpenAPIPath = ""
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	cfg.Info.Description = "Player-facing and game-server-facing HTTP API for ggscale. " +
		"Authenticate with your Game Project's API key (Authorization: Bearer). Game clients " +
		"use the publishable key; game servers and backends use the secret key. Player " +
		"endpoints additionally require a session token in X-Session-Token."
	cfg.Info.Contact = &huma.Contact{
		Name: "ggscale",
		URL:  "https://github.com/automoto/gg-scale",
	}
	cfg.Servers = []*huma.Server{
		{URL: "http://localhost:8080", Description: "Local development server (default HTTP_ADDR)"},
		{
			URL:         "https://{host}",
			Description: "Self-hosted or managed deployment",
			Variables:   map[string]*huma.ServerVariable{"host": {Default: "ggscale.example.com"}},
		},
	}
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"PublishableKey": {
			Type:   "http",
			Scheme: "bearer",
			Description: "Publishable API key of the Game Project: the key you ship in the game client. " +
				"A secret key also works on these operations, but a secret key must never ship in a game.",
		},
		"SecretKey": {
			Type:   "http",
			Scheme: "bearer",
			Description: "Secret API key of the Game Project. Use it only from a game server or backend. " +
				"Operations that list only this scheme refuse a publishable key with 403.",
		},
		"PlayerSession": {
			Type:        "apiKey",
			In:          "header",
			Name:        "X-Session-Token",
			Description: "Player session JWT issued by the /v1/auth endpoints.",
		},
	}
	// Tags group the operations into feature sections in the rendered docs.
	// The order here is the order a reader sees them.
	cfg.Tags = []*huma.Tag{
		{Name: "Authentication", Description: "Player sign-in and session tokens: email/password, anonymous, and custom-token."},
		{Name: "Remote Config", Description: "Project-defined live tuning values available before player login."},
		{Name: "Player Profiles", Description: "Per-project player identity: email and external console id."},
		{Name: "Cloud Saves", Description: "Per-player JSON object storage with optimistic concurrency."},
		{Name: "Leaderboards", Description: "Ranked scoreboards with server-authoritative score submission."},
		{Name: "Friends & Presence", Description: "Friend requests, blocks, and online presence."},
		{Name: "Game Sessions & Invites", Description: "Pre-game rooms with join codes, plus short-lived invites."},
		{Name: "Matchmaking", Description: "Tickets that return a roster, a game session, or a fleet allocation."},
		{Name: "Realtime", Description: "The WebSocket channel for presence, invites, and match events."},
		{Name: "P2P & TURN Relay", Description: "Short-lived TURN credentials for NAT traversal."},
		{Name: "Remote Addresses", Description: "A player's opaque connect handles, such as a Steam id."},
		{Name: "Game Server Fleet", Description: "Dedicated-server heartbeat and fleet listing. Beta."},
		{Name: "Session Verification", Description: "Server-tier check of a player session token."},
		{Name: "Health", Description: "Liveness probe."},
	}
	return cfg
}

// groupAdapter binds a huma API to a chi group that is already mounted under
// /v1. humachi uses op.Path both for the OpenAPI document and for route
// registration; since the group router is scoped to /v1, we strip the /v1
// prefix before registering so the route lands at the right place while the
// spec keeps the full path.
type groupAdapter struct {
	router chi.Router
}

// chiPathMetadata lets an operation override the chi routing pattern (e.g. to
// constrain a path param with a regex) while op.Path stays the spec-facing
// path. Needed where an API path shares a prefix with a mounted page surface.
const chiPathMetadata = "chiPath"

func (a groupAdapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	chiPath := op.Path
	if p, ok := op.Metadata[chiPathMetadata].(string); ok {
		chiPath = p
	}
	routePath := strings.TrimPrefix(chiPath, v1Prefix)
	h := func(w http.ResponseWriter, r *http.Request) {
		handler(humachi.NewContext(op, r, w))
	}
	a.router.MethodFunc(op.Method, routePath, h)
	// The chi Route-based handlers this replaces answered both `/x` and
	// `/x/`; keep the trailing-slash form working so no client breaks. The
	// spec still carries only the canonical (no-slash) op.Path.
	if routePath != "/" {
		a.router.MethodFunc(op.Method, routePath+"/", h)
	}
}

func (a groupAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.router.ServeHTTP(w, r)
}

// groupAPI creates a huma.API bound to the given chi group. Every API built
// from the same cfg shares its OpenAPI document, so operations registered
// across all groups accumulate into one spec.
func groupAPI(r chi.Router, cfg huma.Config) huma.API {
	return huma.NewAPI(cfg, groupAdapter{router: r})
}

// serverError logs err (so an operator can locate the fault) and returns a
// generic problem+json 500 that leaks no internals — the huma equivalent of
// webutil.InternalError. Postgres rejects a NUL byte in a text value with
// SQLSTATE 22021; that is always client input, so it is a 400 instead.
func serverError(ctx context.Context, msg string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22021" {
		return huma.Error400BadRequest("request contains invalid characters")
	}
	slog.ErrorContext(ctx, msg, "error", err)
	return huma.Error500InternalServerError("internal error")
}
