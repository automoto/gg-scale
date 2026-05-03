package dashboard

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	render(r, w, AccountPage(AccountView{CSRFToken: session.CSRFToken}))
}

func (h *Handler) updatePassword(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	current := r.Form.Get("current_password")
	next := r.Form.Get("new_password")
	if len(next) < minDashboardPassLen {
		w.WriteHeader(http.StatusBadRequest)
		render(r, w, AccountPage(AccountView{CSRFToken: session.CSRFToken, Error: "Password must be at least 12 characters"}))
		return
	}

	var row sqlcgen.GetDashboardUserByEmailRow
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetDashboardUserByEmail(r.Context(), session.User.Email)
		return err
	}); err != nil {
		http.Error(w, "account lookup failed", http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(current)) != nil {
		w.WriteHeader(http.StatusUnauthorized)
		render(r, w, AccountPage(AccountView{CSRFToken: session.CSRFToken, Error: "Current password is incorrect"}))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), bcryptCost)
	if err != nil {
		http.Error(w, "password hash failed", http.StatusInternalServerError)
		return
	}
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.UpdateDashboardPassword(r.Context(), sqlcgen.UpdateDashboardPasswordParams{
			ID:           session.User.ID,
			PasswordHash: hash,
		}); err != nil {
			return err
		}
		return q.RevokeAllDashboardSessionsForUser(r.Context(), session.User.ID)
	}); err != nil {
		http.Error(w, "password update failed", http.StatusInternalServerError)
		return
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/v1/dashboard/login", http.StatusSeeOther)
}
