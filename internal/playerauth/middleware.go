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

// ErrRevoked signals that the player no longer exists (deleted). An
// EpochValidator returns it so the middleware can treat the session as invalid
// rather than an infrastructure error.
var ErrRevoked = errors.New("playerauth: session revoked")

// EpochValidator reports a player's current session_epoch. The middleware
// rejects a token whose epoch claim is behind the stored value — the player was
// tenant-banned, disabled, or changed their password after the token was minted
// (all bump the epoch). This makes revocation immediate on every player route,
// not bounded by the access-token TTL.
type EpochValidator interface {
	CurrentEpoch(ctx context.Context, playerID int64) (int64, error)
}

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
// request context already carries a tenant_id. When validator is non-nil the
// middleware also re-checks the player's session_epoch on every request so a
// ban/disable/password-change takes effect immediately (a nil validator skips
// that check, preserving the token-TTL window).
func New(signer *auth.Signer, validator EpochValidator) func(http.Handler) http.Handler {
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

			if validator != nil {
				epoch, verr := validator.CurrentEpoch(r.Context(), claims.PlayerID)
				if errors.Is(verr, ErrRevoked) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if verr != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if claims.SessionEpoch != epoch {
					// Stale epoch: the player was banned/disabled/changed
					// password after this token was minted.
					w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="session revoked"`)
					http.Error(w, "unauthorized", http.StatusUnauthorized)
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
