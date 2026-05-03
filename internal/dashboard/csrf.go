package dashboard

import (
	"crypto/subtle"
	"net/http"
)

const csrfHeader = "X-CSRF-Token"

func (h *Handler) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		session, ok := sessionFromContext(r.Context())
		if !ok {
			http.Error(w, "missing session", http.StatusUnauthorized)
			return
		}
		token := r.Header.Get(csrfHeader)
		if token == "" {
			if !parseForm(w, r) {
				return
			}
			token = r.Form.Get("_csrf")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRFToken)) != 1 {
			http.Error(w, "invalid csrf token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
