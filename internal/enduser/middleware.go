// Package enduser holds the middleware that authenticates an end-user via
// a JWT in the X-Session-Token header. Mount it after tenant.New on routes
// that need the calling player's identity (storage, leaderboards, friends,
// profile). The token's tid claim is checked against the api_key's tenant
// context — a stolen token cannot be replayed under a different tenant's
// api_key.
package enduser

import (
	"context"
	"errors"
	"net/http"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
)

const headerName = "X-Session-Token"

type ctxKey struct{}

// WithID returns a derived context carrying endUserID.
func WithID(ctx context.Context, endUserID int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, endUserID)
}

// IDFromContext extracts the end_user_id installed by the middleware.
// Returns (0, false) when no end-user has been authenticated.
func IDFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxKey{}).(int64)
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

			ctx := WithID(r.Context(), claims.EndUserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
