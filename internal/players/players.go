// Package players implements the server-rendered, player-facing site.
// Identity is the global player account under /v1/players/account/...
// (signup / verify / login / friends / account home). The per-project
// /v1/players/p/{projectID}/... prefix now carries only invite acceptance,
// which links the invitee to their global account; the old per-project
// email/password web auth was superseded by the account flow and removed.
package players

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/signedcookie"
	"github.com/ggscale/ggscale/internal/twofactor"
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
	// TwoFactor encrypts TOTP secrets and signs the 2FA pending cookie.
	// nil = 2FA enrollment unavailable; already-enrolled logins fail closed.
	TwoFactor *twofactor.Cipher
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
	twoFactor        *twofactor.Cipher
}

// New builds the player UI router.
func New(d Deps) http.Handler {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("players: rand: " + err.Error())
	}
	h := &Handler{pool: d.Pool, mailer: d.Mailer, mailFrom: d.MailFrom, cfg: d.Config, now: time.Now, metrics: d.Metrics, verifySigningKey: key, twoFactor: d.TwoFactor}

	r := chi.NewRouter()
	r.Use(webutil.PlayerSecurityHeaders)

	// Global player-account routes (project-agnostic) — the platform-wide
	// account identity: signup / login / verify / friends / account home.
	// See docs/temp/player-accounts.md.
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
		r.Get("/remote-addrs", h.accountRemoteAddrListPage)
		r.Get("/remote-addrs/new", h.accountRemoteAddrNewPage)
		r.Post("/remote-addrs", h.accountRemoteAddrCreate)
		r.Get("/remote-addrs/{slot}/edit", h.accountRemoteAddrEditPage)
		r.Post("/remote-addrs/{slot}", h.accountRemoteAddrUpdateSlot)
		r.Get("/remote-addrs/{slot}/delete", h.accountRemoteAddrDeletePage)
		r.Post("/remote-addrs/{slot}/delete", h.accountRemoteAddrDelete)
		r.Get("/friends", h.friendsPage)
		r.Post("/friends/request", h.friendRequest)
		r.Post("/friends/{accountID}/accept", h.friendAction("accept"))
		r.Post("/friends/{accountID}/reject", h.friendAction("reject"))
		r.Post("/friends/{accountID}/unfriend", h.friendAction("unfriend"))
		r.Post("/friends/{accountID}/block", h.friendAction("block"))
		r.Post("/friends/{accountID}/unblock", h.friendAction("unblock"))
		r.Get("/login", h.accountLoginPage)
		r.Post("/login", h.accountLogin)
		r.Get("/login/2fa", h.accountTwoFactorChallengePage)
		r.Post("/login/2fa", h.accountTwoFactorChallenge)
		r.Get("/2fa", h.accountTwoFactorPage)
		r.Post("/2fa/setup", h.accountTwoFactorSetup)
		r.Post("/2fa/confirm", h.accountTwoFactorConfirm)
		r.Post("/2fa/disable", h.accountTwoFactorDisable)
		r.Post("/2fa/backup-codes", h.accountTwoFactorBackupCodes)
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
		// no session before login/verify, so the control panel's session-bound
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
		// The per-project email/password web auth (login/signup/verify/account)
		// was superseded by the global-account flow (/account/*) and removed.
		// Only the emailed-invite acceptance entry point remains here; it links
		// the invitee to their global account.
		r.Get("/invite/accept", h.inviteAcceptPage)
		r.Post("/invite/accept", h.inviteAcceptHandler)
	})
	return r
}

// csrf is shorthand for the CSRF token pulled off the request context by
// the CSRFCookie middleware. Every render site stamps it onto the view so
// the form's hidden _csrf field can be compared on the matching POST.
func (h *Handler) csrf(r *http.Request) string {
	return webutil.CSRFTokenFromContext(r.Context())
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
// control panel ttl; baked into the signed cookie payload as a server-checked
// expiry so a cookie can't be replayed past it.
const playerVerifyTTL = 30 * time.Minute

// encodeVerifyCookie returns base64(payload) + "." + base64(HMAC-SHA256(key, payload)).
// The JSON payload binds the project and expiry without delimiter ambiguity.
func encodeVerifyCookie(p verifyCookiePayload, key []byte) string {
	payload, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return signedcookie.Sign(key, payload)
}

func decodeVerifyCookie(raw string, key []byte) (verifyCookiePayload, bool) {
	payload, ok := signedcookie.Open(key, raw)
	if !ok {
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
