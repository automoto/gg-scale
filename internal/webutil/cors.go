package webutil

import (
	"net/http"

	"github.com/go-chi/cors"
)

// CORSOptions configures BuildCORS. AllowedOrigins is required in
// production (validated separately by config.Validate); an empty slice in
// dev falls back to wildcard for ergonomics.
type CORSOptions struct {
	AllowedOrigins   []string
	AllowCredentials bool
	// DevWildcard, when true and AllowedOrigins is empty, applies a "*"
	// origin allowlist — only for local dev, never production. config.Validate
	// blocks the prod-empty case before this is invoked.
	DevWildcard bool
}

// BuildCORS returns a chi-compatible CORS middleware honoring AllowedOrigins
// strictly. AllowCredentials and the wildcard fallback are explicit so the
// caller cannot accidentally combine credentials with an unbounded origin
// set (the browser blocks the combination but the config drift would be a
// trap for the next reviewer).
func BuildCORS(opts CORSOptions) func(http.Handler) http.Handler {
	origins := opts.AllowedOrigins
	if len(origins) == 0 && opts.DevWildcard {
		origins = []string{"*"}
	}
	return cors.Handler(cors.Options{
		AllowedOrigins:   origins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-Id", "X-API-Key"},
		ExposedHeaders:   []string{"X-Request-Id"},
		AllowCredentials: opts.AllowCredentials,
		MaxAge:           300,
	})
}
