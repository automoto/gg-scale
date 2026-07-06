package dashboard

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/webutil"
)

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	h.renderAccount(w, r, session, http.StatusOK, nil)
}

func (h *Handler) updatePassword(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	current := r.Form.Get("current_password")
	next := r.Form.Get("new_password")
	if len(next) < minDashboardPassLen {
		h.renderAccount(w, r, session, http.StatusUnprocessableEntity, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{"new_password": "Password must be at least 12 characters"}
		})
		return
	}

	passwordOK, err := h.checkAccountPassword(r.Context(), session.User.Email, current)
	if err != nil {
		http.Error(w, "account lookup failed", http.StatusInternalServerError)
		return
	}
	if !passwordOK {
		h.renderAccount(w, r, session, http.StatusUnauthorized, func(vm *AccountView) {
			vm.FieldErrors = map[string]string{"current_password": "Current password is incorrect"}
		})
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
		if err := q.RevokeAllDashboardSessionsForUser(r.Context(), session.User.ID); err != nil {
			return err
		}
		// A password change is exactly when remembered devices should stop
		// skipping the 2FA challenge.
		if err := h.deleteTrustedDevices(r.Context(), tx, session.User.ID); err != nil {
			return err
		}
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "dashboard.password_change", "", nil)
	}); err != nil {
		http.Error(w, "password update failed", http.StatusInternalServerError)
		return
	}
	h.clearSessionCookie(w)
	htmxRedirect(w, r, pathDashboardLogin)
}
