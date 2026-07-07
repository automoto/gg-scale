package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

// v1Prefix is the path every /v1 operation carries in the OpenAPI document.
// The chi groups the humachi adapters bind to are already mounted under this
// prefix (r.Route("/v1", …)), so groupAdapter strips it before registering the
// route while huma keeps the full path in the spec.
const v1Prefix = "/v1"

// playerSecurity is the security requirement for player-tier operations: a
// tenant API key AND a player session token, matching the frozen spec's
// `security: [{ApiKeyAuth: [], PlayerSession: []}]`.
var playerSecurity = []map[string][]string{{"ApiKeyAuth": {}, "PlayerSession": {}}}

// apiKeySecurity is the requirement for endpoints authenticated by the tenant
// API key alone (player-anonymous), e.g. the /v1/auth/* routes.
var apiKeySecurity = []map[string][]string{{"ApiKeyAuth": {}}}

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
		"Authenticate with a tenant API key (Authorization: Bearer). Player endpoints " +
		"additionally require a session token in X-Session-Token."
	cfg.Info.Contact = &huma.Contact{
		Name: "ggscale",
		URL:  "https://github.com/ggscale/ggscale",
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
		"ApiKeyAuth": {
			Type:        "http",
			Scheme:      "bearer",
			Description: "Tenant API key (publishable or secret).",
		},
		"PlayerSession": {
			Type:        "apiKey",
			In:          "header",
			Name:        "X-Session-Token",
			Description: "Player session JWT issued by the /v1/auth endpoints.",
		},
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

func (a groupAdapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	routePath := strings.TrimPrefix(op.Path, v1Prefix)
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
// webutil.InternalError.
func serverError(ctx context.Context, msg string, err error) error {
	slog.ErrorContext(ctx, msg, "error", err)
	return huma.Error500InternalServerError("internal error")
}
