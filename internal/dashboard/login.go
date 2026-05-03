package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

const (
	bcryptCost          = 12
	loginFailureLimit   = 10
	loginLockoutPeriod  = 15 * time.Minute
	minDashboardPassLen = 12
)

var dummyDashboardBcryptHash = mustGenerateDashboardDummyHash()

type setupInput struct {
	Token    string
	Email    string
	Password string
}

func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	if h.bootstrap == nil || !h.bootstrap.Pending() {
		http.Error(w, "dashboard setup is no longer available", http.StatusGone)
		return
	}
	token := r.URL.Query().Get("token")
	render(r, w, SetupPage(SetupView{Token: token}))
}

func (h *Handler) completeSetup(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	in := setupInput{
		Token:    r.Form.Get("bootstrap_token"),
		Email:    normalizeEmail(r.Form.Get("email")),
		Password: r.Form.Get("password"),
	}
	if err := h.createFirstAdmin(r, in); err != nil {
		status := http.StatusBadRequest
		msg := "Invalid setup request"
		switch {
		case errors.Is(err, errInvalidCredentials):
			status = http.StatusUnauthorized
			msg = "Invalid bootstrap token"
		case errors.Is(err, errBootstrapUnavailable):
			status = http.StatusGone
			msg = "Dashboard setup is no longer available"
		}
		w.WriteHeader(status)
		render(r, w, SetupPage(SetupView{Token: in.Token, Email: in.Email, Error: msg}))
		return
	}
	http.Redirect(w, r, "/v1/dashboard/login", http.StatusSeeOther)
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	render(r, w, LoginPage(LoginView{}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	email := normalizeEmail(r.Form.Get("email"))
	password := r.Form.Get("password")
	user, err := h.authenticate(r, email, password)
	if err != nil {
		status := http.StatusUnauthorized
		msg := "Invalid email or password"
		if errors.Is(err, errLockedAccount) {
			status = http.StatusLocked
			msg = "Account is temporarily locked"
		}
		w.WriteHeader(status)
		render(r, w, LoginPage(LoginView{Email: email, Error: msg}))
		return
	}
	session, err := h.issueSession(r.Context(), w, user.ID, clientIP(r), r.UserAgent())
	if err != nil {
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}
	session.User = user
	http.Redirect(w, r, "/v1/dashboard", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFromContext(r.Context())
	if ok {
		_ = h.revokeSession(r.Context(), session.ID)
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/v1/dashboard/login", http.StatusSeeOther)
}

func (h *Handler) createFirstAdmin(r *http.Request, in setupInput) error {
	if h.bootstrap == nil || !h.bootstrap.Pending() {
		return errBootstrapUnavailable
	}
	if !h.bootstrap.tokenMatches(in.Token) {
		return errInvalidCredentials
	}
	if !validDashboardEmail(in.Email) || len(in.Password) < minDashboardPassLen {
		return errInvalidSignup
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
	if err != nil {
		return fmt.Errorf("setup bcrypt: %w", err)
	}

	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).CreateFirstDashboardAdmin(r.Context(), sqlcgen.CreateFirstDashboardAdminParams{
			Email:        in.Email,
			PasswordHash: hash,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errBootstrapUnavailable
		}
		return err
	})
	if err != nil {
		return err
	}
	h.bootstrap.complete()
	return nil
}

func (h *Handler) authenticate(r *http.Request, email, password string) (dashboardUser, error) {
	if email == "" || password == "" {
		_ = bcrypt.CompareHashAndPassword(dummyDashboardBcryptHash, []byte(password))
		return dashboardUser{}, errInvalidCredentials
	}

	var row sqlcgen.GetDashboardUserByEmailRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetDashboardUserByEmail(r.Context(), email)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyDashboardBcryptHash, []byte(password))
		return dashboardUser{}, errInvalidCredentials
	}
	if err != nil {
		return dashboardUser{}, err
	}
	if row.LockedUntil.Valid && h.now().Before(row.LockedUntil.Time) {
		return dashboardUser{}, errLockedAccount
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) != nil {
		return dashboardUser{}, h.recordLoginFailure(r, row)
	}
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RecordDashboardLoginSuccess(r.Context(), row.ID)
	}); err != nil {
		return dashboardUser{}, err
	}
	return dashboardUser{ID: row.ID, Email: row.Email, IsPlatformAdmin: row.IsPlatformAdmin}, nil
}

func (h *Handler) recordLoginFailure(r *http.Request, row sqlcgen.GetDashboardUserByEmailRow) error {
	failures := row.LoginFailures + 1
	var lockedUntil pgtype.Timestamptz
	if failures >= loginFailureLimit {
		lockedUntil = pgtype.Timestamptz{Time: h.now().Add(loginLockoutPeriod), Valid: true}
	}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).RecordDashboardLoginFailure(r.Context(), sqlcgen.RecordDashboardLoginFailureParams{
			ID:            row.ID,
			LoginFailures: failures,
			LockedUntil:   lockedUntil,
		})
		return err
	})
	if err != nil {
		return err
	}
	return errInvalidCredentials
}

func mustGenerateDashboardDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-dashboard-password-for-timing-equalisation"), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}
