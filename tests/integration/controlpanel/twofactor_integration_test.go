//go:build integration

package controlpanel_test

import (
	"context"
	"encoding/base64"
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

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/mailer"
	_ "github.com/ggscale/ggscale/internal/mailer/noop"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/verifycode"
)

const (
	tfTestPassword = "correct-horse-battery"
	tfLoginPath    = "/v1/control-panel/login"
	tfChallenge    = "/v1/control-panel/login/2fa"

	// testTwoFactorHexKey mirrors internal/controlpanel/twofactor_test.go:15, which stays behind as a unit test.
	testTwoFactorHexKey = "6368616e676520746869732070617373776f726420746f206120736563726574"

	pathControlPanel        = "/v1/control-panel"
	sessionCookieName       = "ggscale_control_panel_session"
	trustedDeviceCookieName = "ggscale_control_panel_trust"
	pathControlPanelAccount = "/v1/control-panel/account/password"
	twoFactorIssuer         = "ggscale control panel"
)

func startTwoFactorDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
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

// newBrowser is an http.Client with a cookie jar that surfaces redirects
// instead of following them, so tests can assert on 303 targets.
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
	u, err := url.Parse(base + pathControlPanel)
	require.NoError(t, err)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

func totpNow(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

// resetTOTPStep simulates the passage of a TOTP period so consecutive
// logins in the same test second don't trip the replay guard.
func resetTOTPStep(t *testing.T, raw *pgxpool.Pool, userID int64) {
	t.Helper()
	_, err := raw.Exec(context.Background(),
		`UPDATE control_panel_user_totp SET last_used_step = 0 WHERE control_panel_user_id = $1`, userID)
	require.NoError(t, err)
}

func latestCSRF(t *testing.T, raw *pgxpool.Pool, userID int64) string {
	t.Helper()
	var secret []byte
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT csrf_secret FROM control_panel_sessions
		 WHERE control_panel_user_id = $1 AND revoked_at IS NULL
		 ORDER BY id DESC LIMIT 1`, userID).Scan(&secret))
	return base64.RawURLEncoding.EncodeToString(secret)
}

func createVerifiedUser(t *testing.T, raw *pgxpool.Pool, email string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(tfTestPassword), bcrypt.MinCost)
	require.NoError(t, err)
	var id int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`INSERT INTO control_panel_users (email, password_hash, email_verified_at)
		 VALUES ($1, $2, now()) RETURNING id`, email, hash).Scan(&id))
	return id
}

func loginForm(email string) url.Values {
	return url.Values{"email": {email}, "password": {tfTestPassword}}
}

func TestControlPanelTwoFactor_fullFlow(t *testing.T) {
	pool, raw := startTwoFactorDB(t)
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{Pool: pool, Config: controlpanel.Config{Mount: true}, Mailer: noopMailer, TwoFactor: cipher}))
	srv := httptest.NewServer(root)
	defer srv.Close()

	userID := createVerifiedUser(t, raw, "op@example.com")
	alice := newBrowser(t)

	// Without 2FA the login lands straight on the control panel with a session.
	resp, _ := tfPostForm(t, alice, srv.URL+tfLoginPath, loginForm("op@example.com"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	require.NotNil(t, jarCookie(t, alice, srv.URL, sessionCookieName))

	// Enroll: setup page carries the manual-entry secret, confirm returns
	// the one-time backup codes.
	csrf := latestCSRF(t, raw, userID)
	resp, body := tfPostForm(t, alice, srv.URL+"/v1/control-panel/account/2fa/setup", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	secretMatch := regexp.MustCompile(`<code>([A-Z2-7 ]+)</code>`).FindStringSubmatch(body)
	require.Len(t, secretMatch, 2, "setup page must show the manual secret")
	secret := strings.ReplaceAll(secretMatch[1], " ", "")

	resp, body = tfPostForm(t, alice, srv.URL+"/v1/control-panel/account/2fa/confirm",
		url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	backupMatches := regexp.MustCompile(`<code>([a-z2-7]{5}-[a-z2-7]{5})</code>`).FindAllStringSubmatch(body, -1)
	require.Len(t, backupMatches, twofactor.BackupCodeCount)
	backupCodes := make([]string, 0, len(backupMatches))
	for _, m := range backupMatches {
		backupCodes = append(backupCodes, m[1])
	}

	// A fresh browser now parks at the challenge with no session issued.
	bob := newBrowser(t)
	resp, _ = tfPostForm(t, bob, srv.URL+tfLoginPath, loginForm("op@example.com"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, tfChallenge, resp.Header.Get("Location"))
	require.Nil(t, jarCookie(t, bob, srv.URL, sessionCookieName))

	getResp, err := bob.Get(srv.URL + tfChallenge)
	require.NoError(t, err)
	require.NoError(t, getResp.Body.Close())
	require.Equal(t, http.StatusOK, getResp.StatusCode)

	// Wrong code is rejected without a session.
	resp, _ = tfPostForm(t, bob, srv.URL+tfChallenge, url.Values{"code": {"000000"}})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Nil(t, jarCookie(t, bob, srv.URL, sessionCookieName))

	// Valid code with "remember this device" issues session + trust cookie.
	resetTOTPStep(t, raw, userID)
	resp, _ = tfPostForm(t, bob, srv.URL+tfChallenge,
		url.Values{"code": {totpNow(t, secret)}, "trust_device": {"1"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	require.NotNil(t, jarCookie(t, bob, srv.URL, sessionCookieName))
	require.NotNil(t, jarCookie(t, bob, srv.URL, trustedDeviceCookieName))

	// The trusted device skips the challenge on the next login.
	resp, _ = tfPostForm(t, bob, srv.URL+tfLoginPath, loginForm("op@example.com"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, pathControlPanel, resp.Header.Get("Location"))

	// A backup code works exactly once.
	carol := newBrowser(t)
	resp, _ = tfPostForm(t, carol, srv.URL+tfLoginPath, loginForm("op@example.com"))
	require.Equal(t, tfChallenge, resp.Header.Get("Location"))
	resp, _ = tfPostForm(t, carol, srv.URL+tfChallenge, url.Values{"code": {backupCodes[0]}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.NotNil(t, jarCookie(t, carol, srv.URL, sessionCookieName))

	dave := newBrowser(t)
	resp, _ = tfPostForm(t, dave, srv.URL+tfLoginPath, loginForm("op@example.com"))
	require.Equal(t, tfChallenge, resp.Header.Get("Location"))
	resp, _ = tfPostForm(t, dave, srv.URL+tfChallenge, url.Values{"code": {backupCodes[0]}})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "spent backup code must not work twice")

	// Repeated failures lock the challenge, and the lockout lapses.
	sawLocked := false
	for range twofactor.MaxAttempts + 1 {
		resp, _ = tfPostForm(t, dave, srv.URL+tfChallenge, url.Values{"code": {"000000"}})
		if resp.StatusCode == http.StatusTooManyRequests {
			sawLocked = true
			break
		}
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
	require.True(t, sawLocked, "challenge must lock after repeated failures")
	resetTOTPStep(t, raw, userID)
	resp, _ = tfPostForm(t, dave, srv.URL+tfChallenge, url.Values{"code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "valid code must not bypass an active lockout")
	_, err = raw.Exec(context.Background(),
		`UPDATE control_panel_user_totp SET locked_until = now() - interval '1 second' WHERE control_panel_user_id = $1`, userID)
	require.NoError(t, err)
	resp, _ = tfPostForm(t, dave, srv.URL+tfChallenge, url.Values{"code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "lapsed lockout must allow a valid code again")

	t.Run("verify_path_cannot_bypass_challenge", func(t *testing.T) {
		verifyUserID := createVerifiedUser(t, raw, "unverified@example.com")
		_, err := raw.Exec(context.Background(),
			`UPDATE control_panel_users SET email_verified_at = NULL WHERE id = $1`, verifyUserID)
		require.NoError(t, err)
		key, err := twofactor.GenerateKey(twoFactorIssuer, "unverified@example.com")
		require.NoError(t, err)
		enc, err := cipher.Encrypt([]byte(key.Secret()))
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`INSERT INTO control_panel_user_totp (control_panel_user_id, secret_enc, confirmed_at) VALUES ($1, $2, now())`,
			verifyUserID, enc)
		require.NoError(t, err)

		erin := newBrowser(t)
		resp, _ := tfPostForm(t, erin, srv.URL+tfLoginPath, loginForm("unverified@example.com"))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		require.Equal(t, "/v1/control-panel/verify", resp.Header.Get("Location"))

		salt, err := verifycode.NewSalt()
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`UPDATE control_panel_users
			 SET email_verification_code_hash = $1, email_verification_salt = $2,
			     email_verification_expires_at = now() + interval '15 minutes'
			 WHERE id = $3`, verifycode.Hash(salt, "123456"), salt, verifyUserID)
		require.NoError(t, err)

		resp, _ = tfPostForm(t, erin, srv.URL+"/v1/control-panel/verify", url.Values{"code": {"123456"}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, tfChallenge, resp.Header.Get("Location"), "verified 2FA user must land on the challenge, not the control panel")
		assert.Nil(t, jarCookie(t, erin, srv.URL, sessionCookieName))

		resetTOTPStep(t, raw, verifyUserID)
		resp, _ = tfPostForm(t, erin, srv.URL+tfChallenge, url.Values{"code": {totpNow(t, key.Secret())}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	})

	t.Run("undecryptable_secret_renders_unavailable", func(t *testing.T) {
		// Simulates an operator changing key material after enrollment: the
		// stored secret was sealed under a key this server doesn't have.
		mismatchID := createVerifiedUser(t, raw, "mismatch@example.com")
		foreignCipher, err := twofactor.NewCipher(strings.Repeat("cd", 32))
		require.NoError(t, err)
		key, err := twofactor.GenerateKey(twoFactorIssuer, "mismatch@example.com")
		require.NoError(t, err)
		enc, err := foreignCipher.Encrypt([]byte(key.Secret()))
		require.NoError(t, err)
		_, err = raw.Exec(context.Background(),
			`INSERT INTO control_panel_user_totp (control_panel_user_id, secret_enc, confirmed_at) VALUES ($1, $2, now())`,
			mismatchID, enc)
		require.NoError(t, err)

		ivan := newBrowser(t)
		resp, _ := tfPostForm(t, ivan, srv.URL+tfLoginPath, loginForm("mismatch@example.com"))
		require.Equal(t, tfChallenge, resp.Header.Get("Location"))
		resp, body := tfPostForm(t, ivan, srv.URL+tfChallenge, url.Values{"code": {totpNow(t, key.Secret())}})
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Contains(t, body, "temporarily unavailable")
		assert.Nil(t, jarCookie(t, ivan, srv.URL, sessionCookieName))
	})

	t.Run("nil_cipher_fails_closed_for_enrolled_user", func(t *testing.T) {
		bare := chi.NewRouter()
		bare.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{Pool: pool, Config: controlpanel.Config{Mount: true}, Mailer: noopMailer}))
		bareSrv := httptest.NewServer(bare)
		defer bareSrv.Close()

		frank := newBrowser(t)
		resp, body := tfPostForm(t, frank, bareSrv.URL+tfLoginPath, loginForm("op@example.com"))
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Contains(t, body, "unavailable")
		assert.Nil(t, jarCookie(t, frank, bareSrv.URL, sessionCookieName))

		// Un-enrolled users still log in; the account page explains that
		// enrollment is off.
		plainID := createVerifiedUser(t, raw, "plain@example.com")
		grace := newBrowser(t)
		resp, _ = tfPostForm(t, grace, bareSrv.URL+tfLoginPath, loginForm("plain@example.com"))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
		_ = plainID
		pageResp, err := grace.Get(bareSrv.URL + pathControlPanelAccount)
		require.NoError(t, err)
		pageBody, err := io.ReadAll(pageResp.Body)
		require.NoError(t, err)
		require.NoError(t, pageResp.Body.Close())
		assert.Contains(t, string(pageBody), "Not available on this server")
	})

	t.Run("disable_restores_plain_login", func(t *testing.T) {
		csrf := latestCSRF(t, raw, userID)
		resetTOTPStep(t, raw, userID)
		resp, body := tfPostForm(t, dave, srv.URL+"/v1/control-panel/account/2fa/disable", url.Values{
			"_csrf":            {csrf},
			"current_password": {tfTestPassword},
			"code":             {totpNow(t, secret)},
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "Two-factor authentication disabled.")

		heidi := newBrowser(t)
		resp, _ = tfPostForm(t, heidi, srv.URL+tfLoginPath, loginForm("op@example.com"))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	})
}

// TestControlPanelTwoFactor_replayedValidCodeDoesNotLock guards the fix for the
// valid-code replay bug: resubmitting a cryptographically valid TOTP whose
// timestep is already spent must be rejected as a bad code but must never
// consume a lockout attempt. Only wrong codes count toward the lockout.
func TestControlPanelTwoFactor_replayedValidCodeDoesNotLock(t *testing.T) {
	pool, raw := startTwoFactorDB(t)
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{Pool: pool, Config: controlpanel.Config{Mount: true}, Mailer: noopMailer, TwoFactor: cipher}))
	srv := httptest.NewServer(root)
	defer srv.Close()

	userID := createVerifiedUser(t, raw, "replay@example.com")

	// Enroll 2FA to obtain the shared secret.
	admin := newBrowser(t)
	resp, _ := tfPostForm(t, admin, srv.URL+tfLoginPath, loginForm("replay@example.com"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	csrf := latestCSRF(t, raw, userID)
	resp, body := tfPostForm(t, admin, srv.URL+"/v1/control-panel/account/2fa/setup", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	secretMatch := regexp.MustCompile(`<code>([A-Z2-7 ]+)</code>`).FindStringSubmatch(body)
	require.Len(t, secretMatch, 2)
	secret := strings.ReplaceAll(secretMatch[1], " ", "")
	resp, _ = tfPostForm(t, admin, srv.URL+"/v1/control-panel/account/2fa/confirm",
		url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// A fresh browser parks at the challenge.
	bob := newBrowser(t)
	resp, _ = tfPostForm(t, bob, srv.URL+tfLoginPath, loginForm("replay@example.com"))
	require.Equal(t, tfChallenge, resp.Header.Get("Location"))

	// Mark every code in the current skew window as already spent, so each
	// submission is a replay the atomic guard rejects (rows == 0).
	_, err = raw.Exec(context.Background(),
		`UPDATE control_panel_user_totp SET last_used_step = $1 WHERE control_panel_user_id = $2`,
		time.Now().Unix()/30+10, userID)
	require.NoError(t, err)

	// Replaying the valid-but-spent code well past the lockout cap must keep
	// returning 401 and never 429: a valid code releases its reserved attempt.
	for i := range twofactor.MaxAttempts + 2 {
		resp, _ = tfPostForm(t, bob, srv.URL+tfChallenge, url.Values{"code": {totpNow(t, secret)}})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"replay #%d must be rejected as a bad code, not locked out", i)
	}
}
