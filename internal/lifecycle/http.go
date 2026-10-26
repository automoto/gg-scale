package lifecycle

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Router is the subset of a router Mount needs. Both *http.ServeMux and
// chi.Router satisfy it.
type Router interface {
	Handle(pattern string, h http.Handler)
}

const (
	readyzPath  = "/readyz"
	statusPath  = "/lifecycle"
	drainPath   = "/lifecycle/drain"
	resumePath  = "/lifecycle/resume"
	routePrefix = statusPath + "/"
)

// Mount registers the lifecycle routes on r:
//
//   - GET /readyz, always open;
//   - GET /lifecycle, behind the token when one is set;
//   - POST /lifecycle/drain and POST /lifecycle/resume, only when a token is
//     set, so an unconfigured process cannot be drained by anyone (fail
//     closed: the routes return 404).
func (c *Controller) Mount(r Router) {
	r.Handle(readyzPath, allowMethod(http.MethodGet, http.HandlerFunc(c.serveReadyz)))
	r.Handle(statusPath, allowMethod(http.MethodGet, RequireBearer(c.token, http.HandlerFunc(c.serveStatus))))
	if c.token == "" {
		return
	}
	r.Handle(drainPath, allowMethod(http.MethodPost, RequireBearer(c.token, http.HandlerFunc(c.serveDrain))))
	r.Handle(resumePath, allowMethod(http.MethodPost, RequireBearer(c.token, http.HandlerFunc(c.serveResume))))
}

// isLifecycleRoute reports whether path is one of the routes Mount registers.
func isLifecycleRoute(path string) bool {
	return path == readyzPath || path == statusPath || strings.HasPrefix(path, routePrefix)
}

func (c *Controller) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	state := c.State()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if state != StateActive {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = io.WriteString(w, string(state)+"\n")
}

func (c *Controller) serveStatus(w http.ResponseWriter, _ *http.Request) {
	writeStatus(w, http.StatusOK, c.Status())
}

func (c *Controller) serveDrain(w http.ResponseWriter, _ *http.Request) {
	c.Drain()
	writeStatus(w, http.StatusOK, c.Status())
}

func (c *Controller) serveResume(w http.ResponseWriter, _ *http.Request) {
	code := http.StatusOK
	if !c.Resume() {
		code = http.StatusConflict
	}
	writeStatus(w, code, c.Status())
}

func writeStatus(w http.ResponseWriter, code int, st Status) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(st)
}

func allowMethod(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireBearer gates next behind "Authorization: Bearer <token>". An empty
// token leaves next open. Both sides are SHA-256'd before the constant-time
// compare so the comparison runs over equal-length digests and does not leak
// the token length through timing.
func RequireBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerCredential(r.Header.Get("Authorization"))
		got := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerCredential extracts the token from an "Authorization: Bearer <token>"
// header. The scheme match is case-insensitive (RFC 7235) and the token is
// whitespace-trimmed.
func bearerCredential(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}
