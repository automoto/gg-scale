package webutil

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// CSRFCookieName is the cookie holding the per-page CSRF nonce. Shared
// across the player site and the control panel invite-accept flow — both are
// anonymous-form surfaces (no session yet) so they can't use a
// session-bound CSRF token like the rest of the control panel.
const CSRFCookieName = "ggscale_csrf"

// CSRFFormField is the conventional hidden form field name carrying the
// nonce. Same constant on both sides so templates and middleware agree.
const CSRFFormField = "_csrf"

const csrfTokenBytes = 32

type csrfContextKey struct{}

// CSRFTokenFromContext returns the per-request CSRF token installed by
// CSRFCookie. Templates call this via the handler to render the hidden
// form field.
func CSRFTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(csrfContextKey{}).(string)
	return v
}

// CSRFConfig parameterises the cookie. Path scopes the cookie to a
// subtree (e.g. "/v1/players/p/42"); Secure follows the deployment's
// HTTPS posture; SameSite is Lax by default so a top-level navigation
// to a GET still carries the cookie.
type CSRFConfig struct {
	Path     string
	Secure   bool
	SameSite http.SameSite
}

// CSRFCookie returns a middleware that ensures every safe request has a
// CSRFCookieName cookie set and exposes its value via context for
// template rendering. Safe methods (GET/HEAD/OPTIONS) mint a token if
// absent; mutating methods leave the cookie alone — RequireCSRF validates
// it against the form/header.
func CSRFCookie(cfg CSRFConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := readCSRFCookie(r)
			if token == "" && isSafeMethod(r.Method) {
				token = mustMintCSRFToken()
				http.SetCookie(w, &http.Cookie{
					Name:     CSRFCookieName,
					Value:    token,
					Path:     cfg.Path,
					HttpOnly: false, // page JS may need it for fetch()
					Secure:   cfg.Secure,
					SameSite: cfg.SameSite,
				})
			}
			if token != "" {
				r = r.WithContext(context.WithValue(r.Context(), csrfContextKey{}, token))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireCSRF enforces the double-submit on mutating methods: the form
// field (or X-CSRF-Token header) must match the CSRFCookieName cookie.
// Safe methods pass through. ParseForm is invoked when the request body
// is form-encoded.
func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		cookieToken := readCSRFCookie(r)
		if cookieToken == "" {
			http.Error(w, "missing csrf cookie", http.StatusForbidden)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			if !ParseForm(w, r) {
				return
			}
			token = r.Form.Get(CSRFFormField)
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(cookieToken)) != 1 {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func readCSRFCookie(r *http.Request) string {
	c, err := r.Cookie(CSRFCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func mustMintCSRFToken() string {
	buf := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		panic("webutil: csrf rand: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}
