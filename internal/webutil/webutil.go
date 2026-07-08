// Package webutil holds the small HTTP helpers shared by every
// server-rendered surface in ggscale: the operator control panel, the
// player-facing site, and the JSON auth handlers in internal/httpapi.
//
// Each of those used to carry private copies of these helpers. Keeping
// one copy here means a fix (e.g. tightening IsUniqueViolation's
// errors.As path) lands once, not three times.
package webutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// BcryptCost is the work factor every ggscale password hash uses.
	// 12 ≈ 250ms on modern hardware — a deliberate per-attempt floor.
	BcryptCost = 12

	// MaxFormBodyBytes caps the body size for HTML form POSTs.
	MaxFormBodyBytes = 1 << 20

	controlPanelCSP = "default-src 'self'; script-src 'self'; script-src-attr 'none'; style-src 'self'; style-src-attr 'none'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	playerCSP       = "default-src 'none'; script-src 'none'; script-src-attr 'none'; style-src 'self'; style-src-attr 'none'; img-src 'self' data:; font-src 'self'; connect-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
)

// SecurityHeaders sets browser-protection headers for the authenticated
// control panel. ControlPanel JS/CSS must be served as first-party static assets.
func SecurityHeaders(next http.Handler) http.Handler {
	return securityHeadersWithCSP(controlPanelCSP, next)
}

// PlayerSecurityHeaders sets a stricter policy for player-facing forms. They
// load first-party stylesheets (Pico + the shared sheet from /v1/assets, with
// their fonts and data: SVG backgrounds) but everything else — script, frames,
// media, connections — stays blocked by default-src 'none'.
func PlayerSecurityHeaders(next http.Handler) http.Handler {
	return securityHeadersWithCSP(playerCSP, next)
}

func securityHeadersWithCSP(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ParseForm parses an HTML form body capped at MaxFormBodyBytes. On
// failure it has already written the response; callers should return.
func ParseForm(w http.ResponseWriter, r *http.Request) bool {
	if r.Form != nil {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

// Render writes a templ component as text/html. On render error it
// slogs the underlying err and writes a 500.
func Render(r *http.Request, w http.ResponseWriter, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render failed", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// InternalError slogs the underlying err and writes a generic 500 so
// internals are not leaked to the user.
func InternalError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// IsUniqueViolation reports whether err is (or wraps) a Postgres 23505
// unique-violation. The substring fallback covers errors wrapped via
// %w by code paths that don't preserve the *pgconn.PgError type.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return strings.Contains(err.Error(), "23505")
}

// RandomHex returns prefix + hex(nbytes random bytes), suitable for
// opaque tokens like refresh tokens and external IDs.
func RandomHex(prefix string, nbytes int) (string, error) {
	buf := make([]byte, nbytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}
