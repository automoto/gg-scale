// Package tenant houses the load-bearing tenant-isolation middleware.
//
// The middleware extracts an API key from the Authorization: Bearer header,
// hashes it with SHA-256, calls the configured Lookup against the api_keys
// table, and injects tenant_id (and optionally project_id) into the request
// context via internal/db.WithTenant / WithProject. Postgres RLS enforces
// the same boundary at the storage layer; failure of either is sufficient
// to block cross-tenant access.
//
// Failure modes:
//   - 401 Unauthorized: missing header, non-Bearer scheme, empty token,
//     or unknown key (lookup returned ErrUnknownKey)
//   - 403 Forbidden: key recognised but revoked
//   - 500 Internal Server Error: unexpected lookup error (DB down etc.)
package tenant

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"

	"github.com/ggscale/ggscale/internal/db"
)

// ErrUnknownKey is returned by a Lookup when no api_keys row matches the
// supplied hash. Distinguished from a real error so the middleware can map
// it to 401 instead of 500.
var ErrUnknownKey = errors.New("tenant: unknown api key")

// Tier is the billing tier of the owning tenant.
type Tier string

// Tier values mirror the CHECK constraint on tenants.tier.
const (
	TierFree    Tier = "free"
	TierPAYG    Tier = "payg"
	TierPremium Tier = "premium"
)

// KeyType splits Stripe-style publishable (embedded in shipped game
// binaries) from secret (game-server / tenant-backend only). Sensitive
// writes — fleet register/heartbeat/deregister, leaderboard submit —
// require KeyTypeSecret. Reads and player-session bootstrap accept either.
type KeyType string

// KeyType values mirror the CHECK constraint on api_keys.key_type.
const (
	KeyTypePublishable KeyType = "publishable"
	KeyTypeSecret      KeyType = "secret"
)

// APIKey is the resolver's view of a row in api_keys joined to its tenant.
type APIKey struct {
	ID        int64
	TenantID  int64
	ProjectID *int64
	Tier      Tier
	Type      KeyType
	Revoked   bool
}

type apiKeyCtxKey struct{}

// WithAPIKey installs k on ctx for downstream middleware (rate-limit,
// audit) to read via APIKeyFromContext.
func WithAPIKey(ctx context.Context, k APIKey) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, k)
}

// APIKeyFromContext returns the APIKey installed by the tenant middleware
// and a boolean indicating whether one was set.
func APIKeyFromContext(ctx context.Context) (APIKey, bool) {
	v, ok := ctx.Value(apiKeyCtxKey{}).(APIKey)
	return v, ok
}

// Lookup resolves an API key by its SHA-256 hash. Implementations must
// return ErrUnknownKey (not nil + nil APIKey) when no row matches.
type Lookup func(ctx context.Context, keyHash []byte) (*APIKey, error)

// New builds the tenant middleware around a Lookup. Mount under /v1/* on
// every route that requires a tenant-authenticated caller.
func New(lookup Lookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			sum := sha256.Sum256([]byte(token))
			key, err := lookup(r.Context(), sum[:])
			if errors.Is(err, ErrUnknownKey) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if key.Revoked {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			ctx := db.WithTenant(r.Context(), key.TenantID)
			if key.ProjectID != nil {
				ctx = db.WithProject(ctx, *key.ProjectID)
			}
			ctx = WithAPIKey(ctx, *key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireKeyType is middleware that returns 403 unless the resolved API
// key's Type matches want. Mount AFTER New so APIKeyFromContext is
// populated. KeyTypeSecret is treated as a strict requirement; callers
// who want "secret-only" routes wrap them in RequireKeyType(KeyTypeSecret).
func RequireKeyType(want KeyType) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if key.Type != want {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
