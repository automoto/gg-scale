// Package players implements the server-rendered, player-facing
// authentication site (signup / verify / login / account / invite-accept).
// It is mounted at /v1/players/p/{projectID}/... — the project ID is
// in the URL so the page doesn't need an api_key bearer to resolve
// tenant context.
//
// Sessions are stored in the same `sessions` table the JSON auth API
// uses; a separate `ggscale_player_session` cookie holds the refresh
// token (HMAC-checked, HttpOnly).
package players

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	sessionTTL              = 30 * 24 * time.Hour
	sessionCookieName       = "ggscale_player_session"
	verifyCookieName        = "ggscale_player_verify"
	verifySubject           = "Your ggscale verification code"
	minPlayerPasswordLength = 8
	maxPlayerPasswordBytes  = 72
)

// bcryptCost is webutil.BcryptCost, re-bound locally so call sites stay
// untouched after the helper extraction.
const bcryptCost = webutil.BcryptCost

// Config controls player site mounting and cookie behavior.
type Config struct {
	Mount        bool
	CookieSecure bool
}

// Enabled reports whether the player site should be mounted.
func (c Config) Enabled() bool { return c.Mount }

// Deps groups everything players.New needs.
type Deps struct {
	Pool     *db.Pool
	Mailer   mailer.Mailer
	MailFrom string
	Config   Config
	// Limiter and Registry may be nil — typically only in unit tests.
	// When nil the per-IP auth rate limiter is skipped; production
	// callers always supply both.
	Limiter ratelimit.Limiter
	// ProxyTrust resolves the real client IP for the per-IP auth limiter when
	// behind a trusted reverse proxy. nil = RemoteAddr only.
	ProxyTrust *ratelimit.ProxyTrust
	Registry   prometheus.Registerer
	// Metrics carries the business counters. nil is a no-op (unit tests).
	Metrics *observability.Metrics
}

// Handler owns player UI HTTP routes.
type Handler struct {
	pool     *db.Pool
	mailer   mailer.Mailer
	mailFrom string
	cfg      Config
	now      func() time.Time
	metrics  *observability.Metrics
	// verifySigningKey signs the short-lived verify-pending cookie.
	// Generated once at handler construction so each process has a fresh
	// secret; restarts invalidate in-flight verify cookies (acceptable —
	// users re-enter from login).
	verifySigningKey []byte
}

// New builds the player UI router.
func New(d Deps) http.Handler {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("players: rand: " + err.Error())
	}
	h := &Handler{pool: d.Pool, mailer: d.Mailer, mailFrom: d.MailFrom, cfg: d.Config, now: time.Now, metrics: d.Metrics, verifySigningKey: key}

	r := chi.NewRouter()
	r.Use(webutil.PlayerSecurityHeaders)

	// Global player-account routes (project-agnostic). These sit alongside
	// the per-project /p/{projectID} routes and drive the platform-wide
	// account identity (signup / login / verify / account home). See
	// docs/temp/player-accounts.md.
	r.Route("/account", func(r chi.Router) {
		if d.Limiter != nil {
			r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, d.Registry))
		}
		r.Use(webutil.CSRFCookie(webutil.CSRFConfig{
			Path:     "/v1/players",
			Secure:   d.Config.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		}))
		r.Use(webutil.RequireCSRF)
		r.Get("/", h.accountHomePage)
		r.Post("/join", h.accountJoin)
		r.Post("/remote-addrs", h.accountRemoteAddrUpdate)
		r.Get("/friends", h.friendsPage)
		r.Post("/friends/request", h.friendRequest)
		r.Post("/friends/{accountID}/accept", h.friendAction("accept"))
		r.Post("/friends/{accountID}/reject", h.friendAction("reject"))
		r.Post("/friends/{accountID}/unfriend", h.friendAction("unfriend"))
		r.Post("/friends/{accountID}/block", h.friendAction("block"))
		r.Post("/friends/{accountID}/unblock", h.friendAction("unblock"))
		r.Get("/login", h.accountLoginPage)
		r.Post("/login", h.accountLogin)
		r.Get("/signup", h.accountSignupPage)
		r.Post("/signup", h.accountSignup)
		r.Get("/verify", h.accountVerifyPage)
		r.Post("/verify", h.accountVerify)
		r.Post("/verify/resend", h.accountVerifyResend)
		r.Post("/logout", h.accountLogout)
	})

	r.Route("/p/{projectID}", func(r chi.Router) {
		if d.Limiter != nil {
			r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, d.Registry))
		}
		// Anonymous-form CSRF (double-submit cookie). The player site has
		// no session before login/verify, so the dashboard's session-bound
		// CSRF token doesn't apply — RequireCSRF + CSRFCookie work
		// together: the cookie middleware mints a per-page nonce on GET,
		// templates render it as a hidden field, RequireCSRF enforces the
		// match on every mutating method.
		r.Use(webutil.CSRFCookie(webutil.CSRFConfig{
			Path:     "/v1/players",
			Secure:   d.Config.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		}))
		r.Use(webutil.RequireCSRF)
		r.Get("/login", h.loginPage)
		r.Post("/login", h.login)
		r.Get("/signup", h.signupPage)
		r.Post("/signup", h.signup)
		r.Get("/verify", h.verifyPage)
		r.Post("/verify", h.verify)
		r.Get("/invite/accept", h.inviteAcceptPage)
		r.Post("/invite/accept", h.inviteAcceptHandler)
		r.Get("/account", h.accountPage)
		r.Post("/logout", h.logout)
	})
	return r
}

// csrf is shorthand for the CSRF token pulled off the request context by
// the CSRFCookie middleware. Every render site stamps it onto the view so
// the form's hidden _csrf field can be compared on the matching POST.
func (h *Handler) csrf(r *http.Request) string {
	return webutil.CSRFTokenFromContext(r.Context())
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	webutil.Render(r, w, LoginPage(LoginView{ProjectID: projectID, CSRFToken: h.csrf(r)}))
}

func (h *Handler) signupPage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	webutil.Render(r, w, SignupPage(SignupView{ProjectID: projectID, CSRFToken: h.csrf(r)}))
}

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	session, ok := h.sessionFromRequest(r)
	if !ok {
		http.Redirect(w, r, playerLoginPath(projectID), http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, AccountPage(AccountView{ProjectID: projectID, Email: session.Email, CSRFToken: h.csrf(r)}))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	if !validPlayerPassword(password) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, LoginPage(LoginView{ProjectID: projectID, Email: email, Error: "Invalid email or password.", CSRFToken: h.csrf(r)}))
		return
	}

	var row sqlcgen.GetPlayerByEmailProjectRow
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var err error
		emailPtr := &email
		row, err = sqlcgen.New(tx).GetPlayerByEmailProject(r.Context(), sqlcgen.GetPlayerByEmailProjectParams{
			ProjectID: projectID,
			Email:     emailPtr,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyPlayerBcryptHash, []byte(password))
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, LoginPage(LoginView{ProjectID: projectID, Email: email, Error: "Invalid email or password.", CSRFToken: h.csrf(r)}))
		return
	}
	if err != nil {
		webutil.InternalError(w, "player login: lookup", err)
		return
	}
	if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(password)) != nil {
		h.metrics.Login(observability.SurfacePlayer, observability.LoginInvalid)
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, LoginPage(LoginView{ProjectID: projectID, Email: email, Error: "Invalid email or password.", CSRFToken: h.csrf(r)}))
		return
	}
	if row.DisabledAt.Valid {
		h.metrics.Login(observability.SurfacePlayer, observability.LoginLocked)
		w.WriteHeader(http.StatusForbidden)
		webutil.Render(r, w, LoginPage(LoginView{ProjectID: projectID, Email: email, Error: "This account has been disabled.", CSRFToken: h.csrf(r)}))
		return
	}
	if !row.EmailVerifiedAt.Valid {
		// Re-mint code if cooldown expired and send the user to verify.
		// A resend cooldown (startVerification returns nil) or a
		// lifetime-lockout (errVerifyAccountLocked) must NOT 500 — the
		// verify screen surfaces the lockout when the user submits a code.
		// Only genuine DB/mail failures are internal errors.
		if err := h.startVerification(r.Context(), row.ID, email); err != nil && !errors.Is(err, errVerifyAccountLocked) {
			webutil.InternalError(w, "player login: verification email", err)
			return
		}
		h.metrics.Login(observability.SurfacePlayer, observability.LoginUnverified)
		h.setVerifyCookie(w, row.ID, email, projectID)
		http.Redirect(w, r, playerVerifyPath(projectID), http.StatusSeeOther)
		return
	}
	if err := h.issueSession(r.Context(), w, row.ID); err != nil {
		webutil.InternalError(w, "player login: session", err)
		return
	}
	h.metrics.Login(observability.SurfacePlayer, observability.LoginOK)
	http.Redirect(w, r, playerAccountPath(projectID), http.StatusSeeOther)
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Form.Get("email")))
	password := r.Form.Get("password")
	view := SignupView{ProjectID: projectID, Email: email, CSRFToken: h.csrf(r)}
	if !validEmail(email) {
		view.FieldErrors = map[string]string{"email": "Enter a valid email."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, SignupPage(view))
		return
	}
	if !validPlayerPassword(password) {
		view.FieldErrors = map[string]string{"password": "Password must be between 8 and 72 characters."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, SignupPage(view))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		webutil.InternalError(w, "player signup: bcrypt", err)
		return
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		webutil.InternalError(w, "player signup: code", err)
		return
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		webutil.InternalError(w, "player signup: salt", err)
		return
	}
	externalID, err := webutil.RandomHex("user_", 16)
	if err != nil {
		webutil.InternalError(w, "player signup: external_id", err)
		return
	}
	codeHash := verifycode.Hash(salt, code)

	var inserted sqlcgen.CreatePlayerRow
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var err error
		emailPtr := &email
		inserted, err = sqlcgen.New(tx).CreatePlayer(r.Context(), sqlcgen.CreatePlayerParams{
			ProjectID:    projectID,
			ExternalID:   externalID,
			Email:        emailPtr,
			PasswordHash: hash,
			CodeHash:     codeHash,
			CodeSalt:     salt,
			ExpiresAt:    pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
		return err
	})
	if err != nil {
		if webutil.IsUniqueViolation(err) {
			view.Error = "An account with that email already exists. Try logging in."
			w.WriteHeader(http.StatusConflict)
			webutil.Render(r, w, SignupPage(view))
			return
		}
		webutil.InternalError(w, "player signup: insert", err)
		return
	}
	h.metrics.Signup(observability.SignupPlayer)

	if h.mailer != nil && h.mailFrom != "" {
		if err := h.mailer.Send(r.Context(), mailer.Message{
			From:    h.mailFrom,
			To:      []string{email},
			Subject: verifySubject,
			Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
		}); err != nil {
			webutil.InternalError(w, "player signup: verification email", err)
			return
		}
	}
	h.setVerifyCookie(w, inserted.ID, email, projectID)
	http.Redirect(w, r, playerVerifyPath(projectID), http.StatusSeeOther)
}

func (h *Handler) verifyPage(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	p, ok := h.verifyCookie(r)
	if !ok || p.ProjectID != projectID {
		http.Redirect(w, r, playerLoginPath(projectID), http.StatusSeeOther)
		return
	}
	webutil.Render(r, w, VerifyPage(VerifyView{ProjectID: projectID, Email: p.Email, CSRFToken: h.csrf(r)}))
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	p, ok := h.verifyCookie(r)
	if !ok || p.ProjectID != projectID {
		http.Redirect(w, r, playerLoginPath(projectID), http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.Form.Get("code"))
	view := VerifyView{ProjectID: projectID, Email: p.Email, CSRFToken: h.csrf(r)}
	if len(code) != 6 {
		view.Error = "Enter the 6-digit code from your email."
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, VerifyPage(view))
		return
	}
	err := h.confirmCode(r.Context(), p.PlayerID, code)
	switch {
	case errors.Is(err, errBadVerifyCode), errors.Is(err, errVerifyExpired):
		h.metrics.Verification(observability.VerifyInvalid)
	case errors.Is(err, errVerifyLocked), errors.Is(err, errVerifyAccountLocked):
		h.metrics.Verification(observability.VerifyThrottled)
	case err == nil:
		h.metrics.Verification(observability.VerifyOK)
	}
	switch {
	case errors.Is(err, errAlreadyVerified):
		h.clearVerifyCookie(w, projectID)
		http.Redirect(w, r, playerLoginPath(projectID), http.StatusSeeOther)
		return
	case errors.Is(err, errBadVerifyCode):
		view.Error = "That code is incorrect. Try again."
		w.WriteHeader(http.StatusUnauthorized)
		webutil.Render(r, w, VerifyPage(view))
		return
	case errors.Is(err, errVerifyExpired):
		view.Error = "That code has expired. Request a new one."
		w.WriteHeader(http.StatusGone)
		webutil.Render(r, w, VerifyPage(view))
		return
	case errors.Is(err, errVerifyLocked):
		view.Error = "Too many attempts. Sign in again to request a fresh code."
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(view))
		return
	case errors.Is(err, errVerifyAccountLocked):
		view.Error = "This account is locked after too many verification attempts. Contact support to unlock."
		w.WriteHeader(http.StatusTooManyRequests)
		webutil.Render(r, w, VerifyPage(view))
		return
	case err != nil:
		webutil.InternalError(w, "player verify", err)
		return
	}
	h.clearVerifyCookie(w, projectID)
	if err := h.issueSession(r.Context(), w, p.PlayerID); err != nil {
		webutil.InternalError(w, "player verify session", err)
		return
	}
	http.Redirect(w, r, playerAccountPath(projectID), http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseProjectID(w, r)
	if !ok {
		return
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		hash := sha256.Sum256([]byte(c.Value))
		_ = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
			return sqlcgen.New(tx).RevokePlayerSession(r.Context(), hash[:])
		})
		h.metrics.PlayerSessionClosed()
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, playerLoginPath(projectID), http.StatusSeeOther)
}

func (h *Handler) startVerification(ctx context.Context, userID int64, email string) error {
	var state sqlcgen.GetPlayerVerificationStateByIDRow
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		state, qerr = sqlcgen.New(tx).GetPlayerVerificationStateByID(ctx, userID)
		return qerr
	})
	if err != nil {
		return err
	}
	if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
		// Locked-out accounts can't refresh their code either; resend
		// would otherwise loop the per-code attempt budget forever.
		return errVerifyAccountLocked
	}
	if !verifycode.CanResend(state.EmailVerificationLastSentAt.Time, h.now()) {
		return nil // silently no-op; user can wait and re-submit
	}
	code, err := verifycode.GenerateCode()
	if err != nil {
		return err
	}
	salt, err := verifycode.NewSalt()
	if err != nil {
		return err
	}
	codeHash := verifycode.Hash(salt, code)
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).SetPlayerVerificationCodeByID(ctx, sqlcgen.SetPlayerVerificationCodeByIDParams{
			ID:        userID,
			CodeHash:  codeHash,
			CodeSalt:  salt,
			ExpiresAt: pgtype.Timestamptz{Time: h.now().Add(verifycode.CodeTTL), Valid: true},
		})
	})
	if err != nil {
		return err
	}
	if h.mailer != nil && h.mailFrom != "" {
		if err := h.mailer.Send(ctx, mailer.Message{
			From:    h.mailFrom,
			To:      []string{email},
			Subject: verifySubject,
			Body:    fmt.Sprintf("Your ggscale verification code is %s (valid 15 minutes).", code),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) confirmCode(ctx context.Context, userID int64, code string) error {
	// locked is set inside the tx when the lifetime cap is crossed; the
	// closure commits (lock + reserve bump persist together) and the
	// outer function surfaces the locked state as an error. Returning the
	// error from inside the closure would roll the tx back and undo
	// both writes.
	var locked bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		state, err := q.GetPlayerVerificationStateByID(ctx, userID)
		if err != nil {
			return err
		}
		if state.EmailVerifiedAt.Valid {
			return errAlreadyVerified
		}
		if state.EmailVerificationLockedUntil.Valid && verifycode.AccountLocked(state.EmailVerificationLockedUntil.Time, h.now()) {
			return errVerifyAccountLocked
		}
		if verifycode.Expired(state.EmailVerificationExpiresAt.Time, h.now()) {
			return errVerifyExpired
		}
		if len(state.EmailVerificationSalt) == 0 || len(state.EmailVerificationCodeHash) == 0 {
			return errVerifyExpired
		}
		// Atomic per-code cap (replaces fetch-then-bump).
		reserved, rerr := q.ReservePlayerVerifyAttempt(ctx, sqlcgen.ReservePlayerVerifyAttemptParams{
			ID:          userID,
			MaxAttempts: int32(verifycode.MaxAttempts),
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return errVerifyLocked
			}
			return rerr
		}
		if verifycode.LifetimeExhausted(int(reserved.EmailVerificationLifetimeAttempts)) {
			lockedUntil := pgtype.Timestamptz{Time: h.now().Add(verifycode.LockoutDuration), Valid: true}
			if lerr := q.LockPlayerVerification(ctx, sqlcgen.LockPlayerVerificationParams{
				ID: userID, LockedUntil: lockedUntil,
			}); lerr != nil {
				return lerr
			}
			locked = true
			return nil
		}
		expected := verifycode.Hash(state.EmailVerificationSalt, code)
		if subtle.ConstantTimeCompare(expected, state.EmailVerificationCodeHash) != 1 {
			return errBadVerifyCode
		}
		return q.MarkPlayerVerifiedByID(ctx, userID)
	})
	if err != nil {
		return err
	}
	if locked {
		return errVerifyAccountLocked
	}
	return nil
}

// playerSession is the public view of a logged-in player.
type playerSession struct {
	UserID    int64
	Email     string
	ProjectID int64
}

func (h *Handler) sessionFromRequest(r *http.Request) (playerSession, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return playerSession{}, false
	}
	hash := sha256.Sum256([]byte(c.Value))
	var (
		out      playerSession
		expires  pgtype.Timestamptz
		revoked  pgtype.Timestamptz
		email    *string
		disabled pgtype.Timestamptz
	)
	err = h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		row, qerr := sqlcgen.New(tx).GetPlayerSession(r.Context(), hash[:])
		if qerr != nil {
			return qerr
		}
		out.UserID = row.PlayerID
		out.ProjectID = row.ProjectID
		expires = row.ExpiresAt
		revoked = row.RevokedAt
		email = row.Email
		disabled = row.DisabledAt
		return nil
	})
	if err != nil {
		return playerSession{}, false
	}
	if revoked.Valid || expires.Time.Before(h.now()) || disabled.Valid {
		return playerSession{}, false
	}
	if email != nil {
		out.Email = *email
	}
	return out, true
}

func (h *Handler) issueSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	refreshToken, err := webutil.RandomHex("", 32)
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(refreshToken))
	expires := h.now().Add(sessionTTL)
	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		// The CreatePlayerSession SQL reads project_players.tenant_id via JOIN
		// to populate sessions.tenant_id; project_players has RLS that
		// requires app.tenant_id. Look the tenant up first via the
		// SECURITY DEFINER project_player_tenant helper (added in
		// migration 0027), then SET app.tenant_id so the JOIN sees the
		// row.
		var tenantID int64
		if err := tx.QueryRow(ctx, "SELECT tenant_id FROM project_player_tenant($1)", userID).Scan(&tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", strconv.FormatInt(tenantID, 10)); err != nil {
			return err
		}
		_, err := sqlcgen.New(tx).CreatePlayerSession(ctx, sqlcgen.CreatePlayerSessionParams{
			PlayerID:    userID,
			RefreshHash: hash[:],
			ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
		})
		return err
	})
	if err != nil {
		return err
	}
	h.metrics.PlayerSessionOpened()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    refreshToken,
		Path:     "/v1/players",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
	return nil
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/v1/players",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

// verifyCookiePayload is the user ID + project ID + email parked while
// the player types the 6-digit code. Signed with a process-local HMAC
// key so an attacker can't forge a payload for another user / project.
// ExpiresAt is a server-checked Unix-seconds expiry — the cookie MaxAge
// is client-controlled and not trusted.
type verifyCookiePayload struct {
	PlayerID  int64
	ProjectID int64
	ExpiresAt int64
	Email     string
	// AccountID is set only for the global-account verify cookie (a UUID
	// string); empty for the per-project player verify cookie.
	AccountID string `json:",omitempty"`
}

// playerVerifyTTL is the lifetime of a verify-pending cookie. Mirrors the
// dashboard ttl; baked into the signed cookie payload as a server-checked
// expiry so a cookie can't be replayed past it.
const playerVerifyTTL = 30 * time.Minute

func (h *Handler) setVerifyCookie(w http.ResponseWriter, userID int64, email string, projectID int64) {
	expiresAt := h.now().Add(playerVerifyTTL).Unix()
	val := encodeVerifyCookie(verifyCookiePayload{PlayerID: userID, ProjectID: projectID, ExpiresAt: expiresAt, Email: email}, h.verifySigningKey)
	http.SetCookie(w, &http.Cookie{
		Name:     verifyCookieName,
		Value:    val,
		Path:     "/v1/players/p/" + strconv.FormatInt(projectID, 10),
		MaxAge:   int(playerVerifyTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) clearVerifyCookie(w http.ResponseWriter, projectID int64) {
	http.SetCookie(w, &http.Cookie{
		Name:     verifyCookieName,
		Value:    "",
		Path:     "/v1/players/p/" + strconv.FormatInt(projectID, 10),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.CookieSecure,
	})
}

func (h *Handler) verifyCookie(r *http.Request) (verifyCookiePayload, bool) {
	c, err := r.Cookie(verifyCookieName)
	if err != nil {
		return verifyCookiePayload{}, false
	}
	p, ok := decodeVerifyCookie(c.Value, h.verifySigningKey)
	if !ok {
		return verifyCookiePayload{}, false
	}
	// MaxAge is advisory; the signed payload's ExpiresAt is authoritative.
	if p.ExpiresAt > 0 && h.now().Unix() > p.ExpiresAt {
		return verifyCookiePayload{}, false
	}
	return p, true
}

// encodeVerifyCookie returns base64(payload) + "." + base64(HMAC-SHA256(key, payload)).
// The JSON payload binds the project and expiry without delimiter ambiguity.
func encodeVerifyCookie(p verifyCookiePayload, key []byte) string {
	payload, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func decodeVerifyCookie(raw string, key []byte) (verifyCookiePayload, bool) {
	encPayload, encSig, ok := strings.Cut(raw, ".")
	if !ok {
		return verifyCookiePayload{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return verifyCookiePayload{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return verifyCookiePayload{}, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
		return verifyCookiePayload{}, false
	}
	var out verifyCookiePayload
	if err := json.Unmarshal(payload, &out); err != nil {
		return verifyCookiePayload{}, false
	}
	return out, true
}

var dummyPlayerBcryptHash = mustGenerateDummyPlayerHash()

func mustGenerateDummyPlayerHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-player-timing-equalisation"), bcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}

func validPlayerPassword(password string) bool {
	return len(password) >= minPlayerPasswordLength && len(password) <= maxPlayerPasswordBytes
}

// Shared errors. Their string forms aren't user-facing — the handlers
// translate them to friendly messages.
var (
	errBadVerifyCode       = errors.New("players: bad verify code")
	errVerifyExpired       = errors.New("players: verify code expired")
	errVerifyLocked        = errors.New("players: verify attempts exhausted")
	errAlreadyVerified     = errors.New("players: account already verified")
	errVerifyAccountLocked = errors.New("players: account locked after too many verification attempts")
)

func parseProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func validEmail(s string) bool {
	_, err := webutil.ValidateEmail(s)
	return err == nil
}

// URL helpers.

func playerLoginPath(projectID int64) string {
	return "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/login"
}

func playerVerifyPath(projectID int64) string {
	return "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/verify"
}

func playerAccountPath(projectID int64) string {
	return "/v1/players/p/" + strconv.FormatInt(projectID, 10) + "/account"
}
