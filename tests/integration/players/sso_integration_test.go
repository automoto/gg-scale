//go:build integration

package players_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/players"
	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/sso/ssotest"
	"github.com/automoto/gg-scale/internal/twofactor"
	"github.com/automoto/gg-scale/internal/webutil"
)

const ssoPath = accountBasePath + "/sso"

type ssoSite struct {
	url  string
	raw  *pgxpool.Pool
	fake *ssotest.Fake
}

// startSSOSite mounts the player site with two fake providers. The second
// one stands in for a later provider so the tests can give one account two
// connections.
func startSSOSite(t *testing.T) ssoSite {
	t.Helper()
	pool, raw := startPlayersDB(t)
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)
	fake := ssotest.New(t)

	// The redirect URL needs the server address, so the handler is bound
	// after the server starts.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	providers := map[string]sso.Provider{}
	for _, name := range []string{"google", "discord"} {
		p := sso.Google("client-id", "client-secret", srv.URL+ssoPath+"/"+name+"/callback")
		p.Name = name
		p.OAuth.Endpoint = fake.Endpoint()
		p.ProfileURL = fake.ProfileURL()
		providers[name] = p
	}
	root := chi.NewRouter()
	root.Mount("/v1/players", players.New(players.Deps{
		Pool:             pool,
		Mailer:           noopMailer,
		MailFrom:         "noreply@test",
		Config:           players.Config{Mount: true, SSOProviders: providers},
		TwoFactor:        cipher,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
	}))
	handler = root
	return ssoSite{url: srv.URL, raw: raw, fake: fake}
}

func (s ssoSite) setGoogleUser(subject, email string, verified bool) {
	v := "false"
	if verified {
		v = "true"
	}
	s.fake.SetProfile(`{"sub":"` + subject + `","email":"` + email + `","email_verified":` + v + `,"name":"Pat Player"}`)
}

func browserGet(t *testing.T, c *http.Client, rawURL string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(rawURL)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

// ssoRoundTrip posts the start form, visits the provider, and returns the
// response of the callback.
func ssoRoundTrip(t *testing.T, c *http.Client, startURL string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, body := tfPostForm(t, c, startURL, form)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	resp, _ = browserGet(t, c, resp.Header.Get("Location"))
	require.Equal(t, http.StatusFound, resp.StatusCode)
	return browserGet(t, c, resp.Header.Get("Location"))
}

func (s ssoSite) signIn(t *testing.T, c *http.Client) (*http.Response, string) {
	t.Helper()
	csrf := primeCSRF(t, c, s.url)
	return ssoRoundTrip(t, c, s.url+ssoPath+"/google/start", url.Values{"_csrf": {csrf}})
}

// passwordSession returns a browser signed in with the test password.
func (s ssoSite) passwordSession(t *testing.T, email string) (*http.Client, string) {
	t.Helper()
	c := newBrowser(t)
	csrf := primeCSRF(t, c, s.url)
	resp, body := tfPostForm(t, c, s.url+accountBasePath+"/login", loginForm(email, csrf))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	require.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	return c, csrf
}

func (s ssoSite) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, s.raw.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (s ssoSite) connections(t *testing.T, email string) int {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM player_account_connections c
		JOIN player_accounts a ON a.id = c.player_account_id WHERE a.email = $1`, email)
}

func (s ssoSite) audits(t *testing.T, action string) int {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM platform_audit_log
		WHERE action = $1 AND actor_service = 'player_sso'`, action)
}

// addConnection links a provider identity to the account directly.
func (s ssoSite) addConnection(t *testing.T, email, provider, subject string) {
	t.Helper()
	_, err := s.raw.Exec(context.Background(),
		`INSERT INTO player_account_connections (player_account_id, provider, subject)
		 SELECT id, $2, $3 FROM player_accounts WHERE email = $1`, email, provider, subject)
	require.NoError(t, err)
}

// createSSOOnlyAccount makes a verified account with no password.
func (s ssoSite) createSSOOnlyAccount(t *testing.T, email, subject string) {
	t.Helper()
	_, err := s.raw.Exec(context.Background(),
		`INSERT INTO player_accounts (email, email_verified_at) VALUES ($1, now())`, email)
	require.NoError(t, err)
	s.addConnection(t, email, "google", subject)
}

func TestPlayerSSO_signin(t *testing.T) {
	s := startSSOSite(t)

	t.Run("should_create_account_on_first_sign_in", func(t *testing.T) {
		s.setGoogleUser("g-new", "New.Player@Example.com", true)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
		assert.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
		assert.NotNil(t, jarCookie(t, c, s.url, accountSessionCookieName))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM player_accounts
			WHERE email = 'new.player@example.com' AND password_hash IS NULL
			  AND email_verified_at IS NOT NULL AND display_name = 'Pat Player'`))
		assert.Equal(t, 1, s.connections(t, "new.player@example.com"))
		assert.Equal(t, 1, s.audits(t, "sso.signup"))
	})

	t.Run("should_sign_in_to_the_linked_account", func(t *testing.T) {
		s.setGoogleUser("g-new", "changed-at-google@example.com", true)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
		assert.NotNil(t, jarCookie(t, c, s.url, accountSessionCookieName))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM player_accounts WHERE email = 'changed-at-google@example.com'`))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM player_account_connections
			WHERE subject = 'g-new' AND last_used_at IS NOT NULL`))
	})

	t.Run("should_show_sign_in_methods_without_password", func(t *testing.T) {
		s.setGoogleUser("g-new", "new.player@example.com", true)
		c := newBrowser(t)
		s.signIn(t, c)

		_, body := browserGet(t, c, s.url+accountBasePath+"/")

		assert.Contains(t, body, "No password is set.")
		assert.Contains(t, body, ssoPath+"/google/unlink")
		assert.Contains(t, body, ssoPath+"/discord/link")
	})

	t.Run("should_reject_password_login_for_account_without_password", func(t *testing.T) {
		c := newBrowser(t)
		csrf := primeCSRF(t, c, s.url)

		resp, _ := tfPostForm(t, c, s.url+accountBasePath+"/login", loginForm("new.player@example.com", csrf))

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Nil(t, jarCookie(t, c, s.url, accountSessionCookieName))
	})

	t.Run("should_reject_disabled_account", func(t *testing.T) {
		s.createSSOOnlyAccount(t, "disabled@example.com", "g-disabled")
		_, err := s.raw.Exec(context.Background(), `UPDATE player_accounts SET disabled_at = now() WHERE email = 'disabled@example.com'`)
		require.NoError(t, err)
		s.setGoogleUser("g-disabled", "disabled@example.com", true)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Contains(t, body, "This account has been disabled.")
		assert.Nil(t, jarCookie(t, c, s.url, accountSessionCookieName))
	})

	t.Run("should_reject_unverified_provider_email", func(t *testing.T) {
		s.setGoogleUser("g-unverified", "unverified@example.com", false)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Contains(t, body, "no verified email address")
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM player_accounts WHERE email = 'unverified@example.com'`))
	})

	t.Run("should_reject_email_of_an_account_with_no_link", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "owner@example.com")
		s.setGoogleUser("g-owner", "owner@example.com", true)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		assert.Equal(t, http.StatusConflict, resp.StatusCode)
		assert.Contains(t, body, "An account with this email exists.")
		assert.Nil(t, jarCookie(t, c, s.url, accountSessionCookieName))
		assert.Equal(t, 0, s.connections(t, "owner@example.com"))
	})

	t.Run("should_report_cancelled_consent", func(t *testing.T) {
		s.fake.SetDeny(true)
		defer s.fake.SetDeny(false)
		c := newBrowser(t)

		resp, body := s.signIn(t, c)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Contains(t, body, "was cancelled")
	})
}

func TestPlayerSSO_signin_requires_two_factor_code(t *testing.T) {
	s := startSSOSite(t)
	s.createSSOOnlyAccount(t, "tf@example.com", "g-tf")
	s.setGoogleUser("g-tf", "tf@example.com", true)
	_, err := s.raw.Exec(context.Background(),
		`INSERT INTO player_account_totp (player_account_id, secret_enc, confirmed_at)
		 SELECT id, '\x00'::bytea, now() FROM player_accounts WHERE email = 'tf@example.com'`)
	require.NoError(t, err)
	c := newBrowser(t)

	resp, body := s.signIn(t, c)

	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	assert.Equal(t, accountChallengePath, resp.Header.Get("Location"))
	assert.Nil(t, jarCookie(t, c, s.url, accountSessionCookieName))
}

func TestPlayerSSO_callback_state(t *testing.T) {
	s := startSSOSite(t)
	s.setGoogleUser("g-state", "state@example.com", true)

	t.Run("should_reject_callback_without_state_cookie", func(t *testing.T) {
		resp, body := browserGet(t, newBrowser(t), s.url+ssoPath+"/google/callback?state=x&code=y")

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, body, "expired or is not valid")
	})

	t.Run("should_reject_replay_of_a_used_callback", func(t *testing.T) {
		c := newBrowser(t)
		csrf := primeCSRF(t, c, s.url)
		resp, _ := tfPostForm(t, c, s.url+ssoPath+"/google/start", url.Values{"_csrf": {csrf}})
		resp, _ = browserGet(t, c, resp.Header.Get("Location"))
		callback := resp.Header.Get("Location")
		first, _ := browserGet(t, c, callback)
		require.Equal(t, http.StatusSeeOther, first.StatusCode)

		second, _ := browserGet(t, c, callback)

		assert.Equal(t, http.StatusBadRequest, second.StatusCode)
	})

	t.Run("should_return_404_for_a_provider_that_is_off", func(t *testing.T) {
		c := newBrowser(t)
		csrf := primeCSRF(t, c, s.url)

		start, _ := tfPostForm(t, c, s.url+ssoPath+"/github/start", url.Values{"_csrf": {csrf}})
		callback, _ := browserGet(t, c, s.url+ssoPath+"/github/callback?state=x&code=y")

		assert.Equal(t, http.StatusNotFound, start.StatusCode)
		assert.Equal(t, http.StatusNotFound, callback.StatusCode)
	})

	t.Run("should_reject_start_without_csrf_token", func(t *testing.T) {
		c := newBrowser(t)
		primeCSRF(t, c, s.url)

		resp, _ := tfPostForm(t, c, s.url+ssoPath+"/google/start", url.Values{})

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("should_allow_form_redirect_to_the_provider_origin", func(t *testing.T) {
		resp, body := browserGet(t, newBrowser(t), s.url+accountBasePath+"/login")

		assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "form-action 'self' "+s.fake.Server.URL)
		assert.Contains(t, body, "Sign in with Google")
	})
}

func TestPlayerSSO_link(t *testing.T) {
	s := startSSOSite(t)
	linkURL := s.url + ssoPath + "/google/link"

	t.Run("should_reject_link_with_wrong_password", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "wrongpw@example.com")
		c, csrf := s.passwordSession(t, "wrongpw@example.com")

		resp, _ := tfPostForm(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {"not-the-password"}})

		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?error=")
	})

	t.Run("should_reject_link_without_session", func(t *testing.T) {
		c := newBrowser(t)
		csrf := primeCSRF(t, c, s.url)

		resp, _ := tfPostForm(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, accountBasePath+"/login", resp.Header.Get("Location"))
	})

	t.Run("should_link_provider_to_the_signed_in_account", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "linker@example.com")
		s.setGoogleUser("g-linker", "other-address@example.com", true)
		c, csrf := s.passwordSession(t, "linker@example.com")

		resp, body := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?flash=")
		assert.Equal(t, 1, s.connections(t, "linker@example.com"))
		assert.Equal(t, 1, s.audits(t, "sso.link"))
	})

	t.Run("should_report_already_linked", func(t *testing.T) {
		s.setGoogleUser("g-linker", "other-address@example.com", true)
		c, csrf := s.passwordSession(t, "linker@example.com")

		resp, _ := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Contains(t, resp.Header.Get("Location"), "already+linked")
		assert.Equal(t, 1, s.connections(t, "linker@example.com"))
	})

	t.Run("should_reject_second_account_of_the_same_provider", func(t *testing.T) {
		s.setGoogleUser("g-linker-second", "second@example.com", true)
		c, csrf := s.passwordSession(t, "linker@example.com")

		resp, _ := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?error=")
		assert.Equal(t, 1, s.connections(t, "linker@example.com"))
	})

	t.Run("should_reject_subject_of_a_different_account_with_audit_row", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "intruder@example.com")
		s.setGoogleUser("g-linker", "other-address@example.com", true)
		c, csrf := s.passwordSession(t, "intruder@example.com")

		resp, _ := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		location, err := url.QueryUnescape(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Contains(t, location, "cannot be linked")
		assert.NotContains(t, location, "linker@example.com")
		assert.Equal(t, 0, s.connections(t, "intruder@example.com"))
		assert.Equal(t, 1, s.audits(t, "sso.link_collision"))
	})

	t.Run("should_reject_callback_in_the_session_of_a_different_account", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "starter@example.com")
		createVerifiedAccount(t, s.raw, "swapped@example.com")
		s.setGoogleUser("g-swap", "swap@example.com", true)
		c, csrf := s.passwordSession(t, "starter@example.com")
		resp, _ := tfPostForm(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})
		resp, _ = browserGet(t, c, resp.Header.Get("Location"))
		callback := resp.Header.Get("Location")
		// The same browser signs in as a different account before the
		// provider sends it back.
		resp, _ = tfPostForm(t, c, s.url+accountBasePath+"/login", loginForm("swapped@example.com", csrf))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)

		resp, _ = browserGet(t, c, callback)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Equal(t, 0, s.connections(t, "starter@example.com"))
		assert.Equal(t, 0, s.connections(t, "swapped@example.com"))
	})
}

func TestPlayerSSO_unlink(t *testing.T) {
	s := startSSOSite(t)
	unlinkURL := s.url + ssoPath + "/google/unlink"

	t.Run("should_unlink_when_a_password_remains", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "haspw@example.com")
		s.addConnection(t, "haspw@example.com", "google", "g-haspw")
		c, csrf := s.passwordSession(t, "haspw@example.com")

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?flash=")
		assert.Equal(t, 0, s.connections(t, "haspw@example.com"))
		assert.Equal(t, 1, s.audits(t, "sso.unlink"))
	})

	t.Run("should_reject_unlink_with_wrong_password", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "guarded@example.com")
		s.addConnection(t, "guarded@example.com", "google", "g-guarded")
		c, csrf := s.passwordSession(t, "guarded@example.com")

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}, "current_password": {"not-the-password"}})

		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?error=")
		assert.Equal(t, 1, s.connections(t, "guarded@example.com"))
	})

	t.Run("should_reject_unlink_of_the_last_sign_in_method", func(t *testing.T) {
		s.createSSOOnlyAccount(t, "only@example.com", "g-only")
		s.setGoogleUser("g-only", "only@example.com", true)
		c := newBrowser(t)
		s.signIn(t, c)
		csrf := jarCookie(t, c, s.url, webutil.CSRFCookieName).Value

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}})

		location, err := url.QueryUnescape(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Contains(t, location, "only sign-in method")
		assert.Equal(t, 1, s.connections(t, "only@example.com"))
	})

	t.Run("should_unlink_when_a_second_connection_remains", func(t *testing.T) {
		s.createSSOOnlyAccount(t, "two@example.com", "g-two")
		s.addConnection(t, "two@example.com", "discord", "d-two")
		s.setGoogleUser("g-two", "two@example.com", true)
		c := newBrowser(t)
		s.signIn(t, c)
		csrf := jarCookie(t, c, s.url, webutil.CSRFCookieName).Value

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}})

		assert.Contains(t, resp.Header.Get("Location"), accountBasePath+"/?flash=")
		assert.Equal(t, 1, s.connections(t, "two@example.com"))
	})

	t.Run("should_report_provider_that_is_not_linked", func(t *testing.T) {
		createVerifiedAccount(t, s.raw, "nolink@example.com")
		c, csrf := s.passwordSession(t, "nolink@example.com")

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		location, err := url.QueryUnescape(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Contains(t, location, "is not linked")
	})
}

func TestPlayerSSO_account_without_password(t *testing.T) {
	s := startSSOSite(t)
	s.createSSOOnlyAccount(t, "nopw@example.com", "g-nopw")
	s.setGoogleUser("g-nopw", "nopw@example.com", true)

	t.Run("should_disable_two_factor_with_the_code_alone", func(t *testing.T) {
		c := newBrowser(t)
		s.signIn(t, c)
		csrf := jarCookie(t, c, s.url, webutil.CSRFCookieName).Value
		_, body := tfPostForm(t, c, s.url+accountTwoFactorPath+"/setup", url.Values{"_csrf": {csrf}})
		secretMatch := regexp.MustCompile(`<code>([A-Z2-7 ]+)</code>`).FindStringSubmatch(body)
		require.Len(t, secretMatch, 2)
		secret := strings.ReplaceAll(secretMatch[1], " ", "")
		resp, _ := tfPostForm(t, c, s.url+accountTwoFactorPath+"/confirm", url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_, page := browserGet(t, c, s.url+accountTwoFactorPath)
		require.NotContains(t, page, `name="current_password"`)
		resetAccountTOTPStep(t, s.raw, "nopw@example.com")

		resp, body = tfPostForm(t, c, s.url+accountTwoFactorPath+"/disable", url.Values{"_csrf": {csrf}, "code": {totpNow(t, secret)}})

		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Contains(t, body, "Two-factor authentication disabled.")
	})

	t.Run("should_set_a_password_through_the_reset_link", func(t *testing.T) {
		token := "reset-token-for-the-account-without-password"
		hash := sha256.Sum256([]byte(token))
		_, err := s.raw.Exec(context.Background(),
			`INSERT INTO player_account_password_resets (player_account_id, token_hash, expires_at)
			 SELECT id, $2, now() + interval '1 hour' FROM player_accounts WHERE email = $1`, "nopw@example.com", hash[:])
		require.NoError(t, err)
		c := newBrowser(t)
		csrf := primeCSRF(t, c, s.url)
		resp, body := tfPostForm(t, c, s.url+accountBasePath+"/reset-password",
			url.Values{"_csrf": {csrf}, "token": {token}, "password": {tfTestPassword}})
		require.Equal(t, http.StatusOK, resp.StatusCode, body)

		resp, _ = tfPostForm(t, c, s.url+accountBasePath+"/login", loginForm("nopw@example.com", csrf))

		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, accountBasePath+"/", resp.Header.Get("Location"))
	})
}
