package controlpanel

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/auditlog"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	bcryptCost             = 12
	loginFailureLimit      = 10
	loginLockoutPeriod     = 15 * time.Minute
	minControlPanelPassLen = 12
)

var dummyControlPanelBcryptHash = mustGenerateControlPanelDummyHash()

type setupInput struct {
	Token    string
	Email    string
	Password string
}

func (h *Handler) setupTokenPage(w http.ResponseWriter, r *http.Request) {
	if h.bootstrap == nil || !h.bootstrap.Pending() {
		http.Error(w, msgSetupUnavailable, http.StatusGone)
		return
	}
	webutil.Render(r, w, SetupTokenPage(SetupTokenView{TokenFilePath: h.bootstrap.TokenFilePath()}))
}

func (h *Handler) verifySetupToken(w http.ResponseWriter, r *http.Request) {
	if h.bootstrap == nil || !h.bootstrap.Pending() {
		http.Error(w, msgSetupUnavailable, http.StatusGone)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	token := r.Form.Get("bootstrap_token")
	if !h.bootstrap.tokenMatches(token) {
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, SetupTokenPage(SetupTokenView{
			TokenFilePath: h.bootstrap.TokenFilePath(),
			FieldErrors:   map[string]string{"bootstrap_token": "Invalid bootstrap token"},
		}))

		return
	}
	webutil.Render(r, w, SetupAdminPage(SetupAdminView{Token: token}))
}

func (h *Handler) completeSetup(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	in := setupInput{
		Token:    r.Form.Get("bootstrap_token"),
		Email:    normalizeEmail(r.Form.Get("email")),
		Password: r.Form.Get("password"),
	}
	user, err := h.createFirstAdmin(r, in)
	if err == nil {
		// The first admin is created unverified. Instead of
		// bouncing them to the login form (a second, confusing sign-in),
		// start verification immediately, park the verify-pending cookie,
		// and land them straight on the verify screen.
		if startErr := h.startVerification(r.Context(), user.ID, user.Email); startErr != nil && !errors.Is(startErr, errVerifyResendTooSoon) {
			if errors.Is(startErr, errVerificationDelivery) {
				slog.ErrorContext(r.Context(), "control panel setup verification delivery", "err", startErr)
				verificationDeliveryUnavailable(w)
				return
			}
			http.Error(w, "verification start failed", http.StatusInternalServerError)
			return
		}
		h.setVerifyPendingCookie(w, verifyPendingPayload{UserID: user.ID, Email: user.Email})
		htmxRedirect(w, r, "/v1/control-panel/verify")
		return
	}
	switch {
	case errors.Is(err, errInvalidCredentials):
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, SetupTokenPage(SetupTokenView{
			TokenFilePath: h.bootstrap.TokenFilePath(),
			Error:         "Bootstrap token no longer valid. Please re-enter it.",
		}))

	case errors.Is(err, errBootstrapUnavailable):
		http.Error(w, msgSetupUnavailable, http.StatusGone)
	case errors.Is(err, errInvalidSignup):
		view := SetupAdminView{Token: in.Token, Email: in.Email, FieldErrors: map[string]string{}}
		if !validControlPanelEmail(in.Email) {
			view.FieldErrors["email"] = "Enter a valid email address"
		}
		if len(in.Password) < minControlPanelPassLen {
			view.FieldErrors["password"] = "Password must be at least 12 characters"
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, SetupAdminPage(view))
	default:
		http.Error(w, "Setup failed", http.StatusInternalServerError)
	}
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	webutil.Render(r, w, LoginPage(LoginView{}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if !webutil.ParseForm(w, r) {
		return
	}
	email := normalizeEmail(r.Form.Get("email"))
	password := r.Form.Get("password")
	user, err := h.authenticate(r, email, password)
	if errors.Is(err, errVerifyRequired) {
		// Password is correct but email isn't verified: mint a fresh code
		// and bounce them to the verify page instead of failing.
		h.metrics.Login(observability.SurfaceControlPanel, observability.LoginUnverified)
		if startErr := h.startVerification(r.Context(), user.ID, user.Email); startErr != nil && !errors.Is(startErr, errVerifyResendTooSoon) {
			if errors.Is(startErr, errVerificationDelivery) {
				slog.ErrorContext(r.Context(), "control panel login verification delivery", "err", startErr)
				verificationDeliveryUnavailable(w)
				return
			}
			http.Error(w, "verification start failed", http.StatusInternalServerError)
			return
		}
		h.setVerifyPendingCookie(w, verifyPendingPayload{UserID: user.ID, Email: user.Email})
		htmxRedirect(w, r, "/v1/control-panel/verify")
		return
	}
	if err != nil {
		status := http.StatusUnauthorized
		msg := "Invalid email or password"
		result := observability.LoginInvalid
		if errors.Is(err, errLockedAccount) {
			status = http.StatusLocked
			msg = "Account is temporarily locked"
			result = observability.LoginLocked
		}
		h.metrics.Login(observability.SurfaceControlPanel, result)
		w.WriteHeader(status)
		webutil.Render(r, w, LoginPage(LoginView{Email: email, Error: msg}))
		return
	}
	h.finishLogin(w, r, user)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFromContext(r.Context())
	if ok {
		_ = h.revokeSession(r.Context(), session.ID)
		_ = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return auditlog.WritePlatform(r.Context(), tx, session.User.ID, "control_panel.logout", session.User.Email, nil)
		})
	}
	h.clearSessionCookie(w)
	htmxRedirect(w, r, pathControlPanelLogin)
}

func (h *Handler) createFirstAdmin(r *http.Request, in setupInput) (controlPanelUser, error) {
	if h.bootstrap == nil || !h.bootstrap.Pending() {
		return controlPanelUser{}, errBootstrapUnavailable
	}
	if !h.bootstrap.tokenMatches(in.Token) {
		return controlPanelUser{}, errInvalidCredentials
	}
	if !validControlPanelEmail(in.Email) || len(in.Password) < minControlPanelPassLen {
		return controlPanelUser{}, errInvalidSignup
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcryptCost)
	if err != nil {
		return controlPanelUser{}, fmt.Errorf("setup bcrypt: %w", err)
	}

	var created sqlcgen.CreateFirstControlPanelAdminRow
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var qerr error
		created, qerr = sqlcgen.New(tx).CreateFirstControlPanelAdmin(r.Context(), sqlcgen.CreateFirstControlPanelAdminParams{
			Email:        in.Email,
			PasswordHash: hash,
		})
		if errors.Is(qerr, pgx.ErrNoRows) {
			return errBootstrapUnavailable
		}
		if qerr != nil {
			return qerr
		}
		if h.rbac != nil {
			if gerr := h.rbac.AddPlatformAdminTx(r.Context(), tx, created.ID); gerr != nil {
				return fmt.Errorf("setup platform admin grant: %w", gerr)
			}
		}
		return nil
	})
	if err != nil {
		return controlPanelUser{}, err
	}
	h.bootstrap.complete()
	h.reloadRBACPolicy(r.Context())
	h.metrics.Signup(observability.SignupControlPanelUser)
	return controlPanelUser{ID: created.ID, Email: created.Email, IsPlatformAdmin: created.IsPlatformAdmin}, nil
}

func (h *Handler) authenticate(r *http.Request, email, password string) (controlPanelUser, error) {
	if email == "" || password == "" {
		_ = bcrypt.CompareHashAndPassword(dummyControlPanelBcryptHash, []byte(password))
		return controlPanelUser{}, errInvalidCredentials
	}

	var row sqlcgen.GetControlPanelUserByEmailRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetControlPanelUserByEmail(r.Context(), email)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyControlPanelBcryptHash, []byte(password))
		return controlPanelUser{}, errInvalidCredentials
	}
	if err != nil {
		return controlPanelUser{}, err
	}
	if row.LockedUntil.Valid && h.now().Before(row.LockedUntil.Time) {
		return controlPanelUser{}, errLockedAccount
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) != nil {
		return controlPanelUser{}, h.recordLoginFailure(r, row)
	}
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RecordControlPanelLoginSuccess(r.Context(), row.ID)
	}); err != nil {
		return controlPanelUser{}, err
	}
	if !row.EmailVerifiedAt.Valid {
		return controlPanelUser{ID: row.ID, Email: row.Email, IsPlatformAdmin: row.IsPlatformAdmin}, errVerifyRequired
	}
	return controlPanelUser{ID: row.ID, Email: row.Email, IsPlatformAdmin: row.IsPlatformAdmin}, nil
}

var errVerifyRequired = errors.New("control panel: verify required")

func (h *Handler) recordLoginFailure(r *http.Request, row sqlcgen.GetControlPanelUserByEmailRow) error {
	// Compute the lockout-until timestamp the SQL CASE branches on. The
	// branch only fires when the increment would tip the row over
	// loginFailureLimit, so the value is only consulted at that boundary.
	lockoutUntil := pgtype.Timestamptz{Time: h.now().Add(loginLockoutPeriod), Valid: true}
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).BumpControlPanelLoginFailure(r.Context(), sqlcgen.BumpControlPanelLoginFailureParams{
			ID:           row.ID,
			FailureLimit: int32(loginFailureLimit),
			LockoutUntil: lockoutUntil,
		})
		return err
	})
	if err != nil {
		return err
	}
	return errInvalidCredentials
}

func mustGenerateControlPanelDummyHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-control panel-password-for-timing-equalisation"), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}
