// Package playerauth holds the middleware that authenticates a player via
// a JWT in the X-Session-Token header. Mount it after tenant.New on routes
// that need the calling player's identity (storage, leaderboards, friends,
// profile). The token's tid claim is checked against the api_key's tenant
// context — a stolen token cannot be replayed under a different tenant's
// api_key.
package playerauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
)

const headerName = "X-Session-Token"

type ctxKey struct{}
type projectCtxKey struct{}

// WithID returns a derived context carrying playerID.
func WithID(ctx context.Context, playerID int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, playerID)
}

// WithProjectID returns a derived context carrying the project id from the
// verified session token. A zero project id is treated as absent.
func WithProjectID(ctx context.Context, projectID int64) context.Context {
	return context.WithValue(ctx, projectCtxKey{}, projectID)
}

// IDFromContext extracts the player_id installed by the middleware.
// Returns (0, false) when no player has been authenticated.
func IDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKey{}).(int64)
	if !ok || v == 0 {
		return 0, false
	}
	return v, true
}

// ProjectIDFromContext extracts the project_id claim installed by the
// middleware. Returns (0, false) for legacy tokens with no project pin.
func ProjectIDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(projectCtxKey{}).(int64)
	if !ok || v == 0 {
		return 0, false
	}
	return v, true
}

// New builds the middleware. The tenant middleware must run first so the
// request context already carries a tenant_id.
func New(signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, err := db.TenantFromContext(r.Context())
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			tok := r.Header.Get(headerName)
			if tok == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			claims, err := signer.Verify(tok)
			if errors.Is(err, auth.ErrTokenExpired) {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token expired"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			if claims.TenantID != tenantID {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			// When the api_key is project-pinned, also assert the
			// session's pid claim matches. Closes a same-tenant cross-
			// project session-replay seam: a session minted under
			// project A must not work when presented under an api_key
			// pinned to project B.
			if projectID, ok := db.ProjectFromContext(r.Context()); ok {
				if claims.ProjectID != projectID {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}

			ctx := WithID(r.Context(), claims.PlayerID)
			if claims.ProjectID != 0 {
				ctx = WithProjectID(ctx, claims.ProjectID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
