//go:build integration

package players

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"golang.org/x/crypto/bcrypt"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/mailer"
	_ "github.com/ggscale/ggscale/internal/mailer/noop"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/verifycode"
	"github.com/ggscale/ggscale/internal/webutil"
)

const tfTestPassword = "player-password-1"

func startPlayersDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase("ggscale_test"),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(shutdownCtx)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	return db.NewPool(raw), raw
}

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func tfPostForm(t *testing.T, c *http.Client, rawURL string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(rawURL, form)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

func jarCookie(t *testing.T, c *http.Client, base, name string) *http.Cookie {
	t.Helper()
	u, err := url.Parse(base + accountBasePath)
	require.NoError(t, err)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

// primeCSRF GETs the login page so the double-submit cookie lands in the
// jar, then returns its value for the _csrf form field.
func primeCSRF(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	resp, err := c.Get(base + accountBasePath + "/login")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	ck := jarCookie(t, c, base, webutil.CSRFCookieName)
	require.NotNil(t, ck, "login page must mint the csrf cookie")
	return ck.Value
}

func totpNow(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

func resetAccountTOTPStep(t *testing.T, raw *pgxpool.Pool, email string) {
	t.Helper()
	_, err := raw.Exec(context.Background(),
		`UPDATE player_account_totp SET last_used_step = 0
		 WHERE player_account_id = (SELECT id FROM player_accounts WHERE email = $1)`, email)
	require.NoError(t, err)
}

func createVerifiedAccount(t *testing.T, raw *pgxpool.Pool, email string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(tfTestPassword), bcrypt.MinCost)
	require.NoError(t, err)
	_, err = raw.Exec(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ($1, $2, now())`, email, hash)
	require.NoError(t, err)
}

func loginForm(email, csrf string) url.Values {
	return url.Values{"email": {email}, "password": {tfTestPassword}, "_csrf": {csrf}}
}

func TestPlayerAccountTwoFactor_fullFlow(t *testing.T) {
	pool, raw := startPlayersDB(t)
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount("/v1/players", New(Deps{Pool: pool, Mailer: noopMailer, MailFrom: "noreply@test", Config: Config{Mount: true}, TwoFactor: cipher}))
	srv := httptest.NewServer(root)
	defer srv.Close()

	createVerifiedAccount(t, raw, "player@example.com")
	loginURL := srv.URL + accountBasePath + "/login"
	challengeURL := srv.URL + accountChallengePath

	// Login without 2FA lands on the account home with a session.
	alice := newBrowser(t)
	csrf := primeCSRF(t, alice, srv.URL)
	resp, _ := tfPostForm(t, alice, loginURL, loginForm("player@example.com", csrf))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	require.NotNil(t, jarCookie(t, alice, srv.URL, accountSessionCookieName))

	// Enroll via the security pages.
	resp, body := tfPostForm(t, alice, srv.URL+accountTwoFactorPath+"/setup", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	secretMatch := regexp.MustCompile(`<code>([A-Z2-7 ]+)</code>`).FindStringSubmatch(body)
	require.Len(t, secretMatch, 2, "setup page must show the manual secret")
	secret := strings.ReplaceAll(secretMatch[1], " ", "")

	resp, body = tfPostForm(t, alice, srv.URL+accountTwoFactorPath+"/confirm",
		url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	backupMatches := regexp.MustCompile(`<code>([a-z2-7]{5}-[a-z2-7]{5})</code>`).FindAllStringSubmatch(body, -1)
	require.Len(t, backupMatches, twofactor.BackupCodeCount)
	backupCode := backupMatches[0][1]

	// A fresh browser parks at the challenge with no session.
	bob := newBrowser(t)
	bobCSRF := primeCSRF(t, bob, srv.URL)
	resp, _ = tfPostForm(t, bob, loginURL, loginForm("player@example.com", bobCSRF))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, accountChallengePath, resp.Header.Get("Location"))
	require.Nil(t, jarCookie(t, bob, srv.URL, accountSessionCookieName))

	// The challenge POST is CSRF-protected like every player form.
	resp, _ = tfPostForm(t, bob, challengeURL, url.Values{"code": {"000000"}})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "missing _csrf must be rejected")

	// Wrong code is rejected; valid code with trust issues session + cookie.
	resp, _ = tfPostForm(t, bob, challengeURL, url.Values{"_csrf": {bobCSRF}, "code": {"000000"}})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resetAccountTOTPStep(t, raw, "player@example.com")
	resp, _ = tfPostForm(t, bob, challengeURL,
		url.Values{"_csrf": {bobCSRF}, "code": {totpNow(t, secret)}, "trust_device": {"1"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	require.NotNil(t, jarCookie(t, bob, srv.URL, accountSessionCookieName))
	require.NotNil(t, jarCookie(t, bob, srv.URL, accountTrustCookieName))

	// Trusted device skips the challenge next time.
	resp, _ = tfPostForm(t, bob, loginURL, loginForm("player@example.com", bobCSRF))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))

	// A backup code works exactly once.
	carol := newBrowser(t)
	carolCSRF := primeCSRF(t, carol, srv.URL)
	resp, _ = tfPostForm(t, carol, loginURL, loginForm("player@example.com", carolCSRF))
	require.Equal(t, accountChallengePath, resp.Header.Get("Location"))
	resp, _ = tfPostForm(t, carol, challengeURL, url.Values{"_csrf": {carolCSRF}, "code": {backupCode}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NotNil(t, jarCookie(t, carol, srv.URL, accountSessionCookieName))

	dave := newBrowser(t)
	daveCSRF := primeCSRF(t, dave, srv.URL)
	resp, _ = tfPostForm(t, dave, loginURL, loginForm("player@example.com", daveCSRF))
	require.Equal(t, accountChallengePath, resp.Header.Get("Location"))
	resp, _ = tfPostForm(t, dave, challengeURL, url.Values{"_csrf": {daveCSRF}, "code": {backupCode}})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "spent backup code must not work twice")

	t.Run("verify_path_cannot_bypass_challenge", func(t *testing.T) {
		createVerifiedAccount(t, raw, "unverified@example.com")
		var accountEmail = "unverified@example.com"
		_, err := raw.Exec(context.Background(),
			`UPDATE player_accounts SET email_verified_at = NULL WHERE email = $1`, accountEmail)
		require.NoError(t, err)
		key, err := twofactor.GenerateKey(playerTwoFactorIssuer, accountEmail)
		require.NoError(t, err)
		enc, err := cipher.Encrypt([]byte(key.Secret()))
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`INSERT INTO player_account_totp (player_account_id, secret_enc, confirmed_at)
			 SELECT id, $1, now() FROM player_accounts WHERE email = $2`, enc, accountEmail)
		require.NoError(t, err)

		erin := newBrowser(t)
		erinCSRF := primeCSRF(t, erin, srv.URL)
		resp, _ := tfPostForm(t, erin, loginURL, loginForm(accountEmail, erinCSRF))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		require.Equal(t, accountBasePath+"/verify", resp.Header.Get("Location"))

		salt, err := verifycode.NewSalt()
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`UPDATE player_accounts
			 SET email_verification_code_hash = $1, email_verification_salt = $2,
			     email_verification_expires_at = now() + interval '15 minutes'
			 WHERE email = $3`, verifycode.Hash(salt, "123456"), salt, accountEmail)
		require.NoError(t, err)

		resp, _ = tfPostForm(t, erin, srv.URL+accountBasePath+"/verify",
			url.Values{"_csrf": {erinCSRF}, "code": {"123456"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, accountChallengePath, resp.Header.Get("Location"), "verified 2FA player must land on the challenge, not the account home")
		assert.Nil(t, jarCookie(t, erin, srv.URL, accountSessionCookieName))

		resetAccountTOTPStep(t, raw, accountEmail)
		resp, _ = tfPostForm(t, erin, challengeURL,
			url.Values{"_csrf": {erinCSRF}, "code": {totpNow(t, key.Secret())}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	})

	t.Run("undecryptable_secret_renders_unavailable", func(t *testing.T) {
		createVerifiedAccount(t, raw, "mismatch@example.com")
		foreignCipher, err := twofactor.NewCipher(strings.Repeat("cd", 32))
		require.NoError(t, err)
		key, err := twofactor.GenerateKey(playerTwoFactorIssuer, "mismatch@example.com")
		require.NoError(t, err)
		enc, err := foreignCipher.Encrypt([]byte(key.Secret()))
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`INSERT INTO player_account_totp (player_account_id, secret_enc, confirmed_at)
			 SELECT id, $1, now() FROM player_accounts WHERE email = 'mismatch@example.com'`, enc)
		require.NoError(t, err)

		ivan := newBrowser(t)
		ivanCSRF := primeCSRF(t, ivan, srv.URL)
		resp, _ := tfPostForm(t, ivan, loginURL, loginForm("mismatch@example.com", ivanCSRF))
		require.Equal(t, accountChallengePath, resp.Header.Get("Location"))
		resp, body := tfPostForm(t, ivan, challengeURL,
			url.Values{"_csrf": {ivanCSRF}, "code": {totpNow(t, key.Secret())}})
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Contains(t, body, "temporarily unavailable")
		assert.Nil(t, jarCookie(t, ivan, srv.URL, accountSessionCookieName))
	})

	t.Run("disable_restores_plain_login", func(t *testing.T) {
		resetAccountTOTPStep(t, raw, "player@example.com")
		resp, body := tfPostForm(t, carol, srv.URL+accountTwoFactorPath+"/disable", url.Values{
			"_csrf":            {carolCSRF},
			"current_password": {tfTestPassword},
			"code":             {totpNow(t, secret)},
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "Two-factor authentication disabled.")

		frank := newBrowser(t)
		frankCSRF := primeCSRF(t, frank, srv.URL)
		resp, _ = tfPostForm(t, frank, loginURL, loginForm("player@example.com", frankCSRF))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	})
}

// TestPlayerAccountTwoFactor_replayedValidCodeDoesNotLock guards the fix for
// the valid-code replay bug: resubmitting a cryptographically valid TOTP whose
// timestep is already spent must be rejected as a bad code but must never
// consume a lockout attempt. Only wrong codes count toward the lockout.
func TestPlayerAccountTwoFactor_replayedValidCodeDoesNotLock(t *testing.T) {
	pool, raw := startPlayersDB(t)
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount("/v1/players", New(Deps{Pool: pool, Mailer: noopMailer, MailFrom: "noreply@test", Config: Config{Mount: true}, TwoFactor: cipher}))
	srv := httptest.NewServer(root)
	defer srv.Close()

	createVerifiedAccount(t, raw, "replay@example.com")
	loginURL := srv.URL + accountBasePath + "/login"
	challengeURL := srv.URL + accountChallengePath

	// Enroll 2FA to obtain the shared secret.
	alice := newBrowser(t)
	csrf := primeCSRF(t, alice, srv.URL)
	resp, _ := tfPostForm(t, alice, loginURL, loginForm("replay@example.com", csrf))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, body := tfPostForm(t, alice, srv.URL+accountTwoFactorPath+"/setup", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	secretMatch := regexp.MustCompile(`<code>([A-Z2-7 ]+)</code>`).FindStringSubmatch(body)
	require.Len(t, secretMatch, 2)
	secret := strings.ReplaceAll(secretMatch[1], " ", "")
	resp, _ = tfPostForm(t, alice, srv.URL+accountTwoFactorPath+"/confirm",
		url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// A fresh browser parks at the challenge.
	bob := newBrowser(t)
	bobCSRF := primeCSRF(t, bob, srv.URL)
	resp, _ = tfPostForm(t, bob, loginURL, loginForm("replay@example.com", bobCSRF))
	require.Equal(t, accountChallengePath, resp.Header.Get("Location"))

	// Mark every code in the current skew window as already spent, so each
	// submission is a replay the atomic guard rejects (rows == 0).
	_, err = raw.Exec(context.Background(),
		`UPDATE player_account_totp SET last_used_step = $1
		 WHERE player_account_id = (SELECT id FROM player_accounts WHERE email = $2)`,
		time.Now().Unix()/30+10, "replay@example.com")
	require.NoError(t, err)

	// Replaying the valid-but-spent code well past the lockout cap must keep
	// returning 401 and never 429: a valid code releases its reserved attempt.
	for i := range twofactor.MaxAttempts + 2 {
		resp, _ = tfPostForm(t, bob, challengeURL,
			url.Values{"_csrf": {bobCSRF}, "code": {totpNow(t, secret)}})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"replay #%d must be rejected as a bad code, not locked out", i)
	}
}
