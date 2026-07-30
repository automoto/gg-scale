package players

// One-click invite-email unsubscribe (platform-wide suppression list).
// Mounted at /v1/unsubscribe independently of the player site so the link in
// an invite email always resolves. Signed-out by design: the HMAC token in
// the link is the authorization. The POST also accepts the RFC 8058
// one-click form mail clients send to the List-Unsubscribe URL.

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

// NewUnsubscribeHandler serves the signed-out unsubscribe confirmation page
// (GET) and the suppression write (POST).
func NewUnsubscribeHandler(pool *db.Pool, signingKey []byte) http.Handler {
	h := &unsubscribeHandler{pool: pool, signingKey: append([]byte(nil), signingKey...)}
	r := chi.NewRouter()
	r.Use(webutil.PlayerSecurityHeaders)
	r.Get("/", h.page)
	r.Post("/", h.unsubscribe)
	return r
}

type unsubscribeHandler struct {
	pool       *db.Pool
	signingKey []byte
}

func (h *unsubscribeHandler) email(r *http.Request) (string, string, bool) {
	token := r.URL.Query().Get("token")
	email, ok := webutil.ParseEmailUnsubscribeToken(h.signingKey, token)
	return email, token, ok
}

func (h *unsubscribeHandler) page(w http.ResponseWriter, r *http.Request) {
	email, token, ok := h.email(r)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		webutil.Render(r, w, UnsubscribeInvalidPage())
		return
	}
	webutil.Render(r, w, UnsubscribePage(UnsubscribeView{Email: email, Token: token}))
}

func (h *unsubscribeHandler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	email, _, ok := h.email(r)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		webutil.Render(r, w, UnsubscribeInvalidPage())
		return
	}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SuppressEmail(r.Context(), strings.ToLower(strings.TrimSpace(email)))
	})
	if err != nil {
		webutil.InternalError(w, "unsubscribe", err)
		return
	}
	webutil.Render(r, w, UnsubscribeDonePage(UnsubscribeView{Email: email}))
}
