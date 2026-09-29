//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/sso/ssotest"
	"github.com/automoto/gg-scale/internal/twofactor"
	"github.com/automoto/gg-scale/internal/verifycode"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	ssoCallbackPath  = pathControlPanel + "/sso"
	ssoLoginStart    = tfLoginPath + "/sso/google/start"
	inviteAcceptPath = pathControlPanel + "/invite/accept"
	signupAcceptPath = pathControlPanel + "/request-access/accept"
	accountSSOPath   = pathControlPanel + "/account/sso"
	ssoStateCookie   = "ggscale_control_panel_sso"
)

type ssoPanel struct {
	url       string
	raw       *pgxpool.Pool
	fake      *ssotest.Fake
	providers map[string]sso.Provider
}

// startSSOPanel mounts the control panel with two fake providers. The
// second one stands in for a later provider so the tests can give one user
// two connections.
func startSSOPanel(t *testing.T) ssoPanel {
	t.Helper()
	pool, raw := startTwoFactorDB(t)
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
	for _, name := range []string{"google", "github"} {
		p := sso.Google("client-id", "client-secret", srv.URL+ssoCallbackPath+"/"+name+"/callback")
		p.Name = name
		p.OAuth.Endpoint = fake.Endpoint()
		p.ProfileURL = fake.ProfileURL()
		providers[name] = p
	}
	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool:             pool,
		Config:           controlpanel.Config{Mount: true, SSOProviders: providers},
		Mailer:           noopMailer,
		TwoFactor:        cipher,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
	}))
	handler = root
	return ssoPanel{url: srv.URL, raw: raw, fake: fake, providers: providers}
}

func (s ssoPanel) setGoogleUser(subject, email string) {
	s.fake.SetProfile(`{"sub":"` + subject + `","email":"` + email + `","email_verified":true,"name":"Op Erator"}`)
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
func ssoRoundTrip(t *testing.T, c *http.Client, startURL string, form url.Values) *http.Response {
	t.Helper()
	resp, body := tfPostForm(t, c, startURL, form)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	resp, _ = browserGet(t, c, resp.Header.Get("Location"))
	require.Equal(t, http.StatusFound, resp.StatusCode)
	resp, _ = browserGet(t, c, resp.Header.Get("Location"))
	return resp
}

func (s ssoPanel) signIn(t *testing.T, c *http.Client) *http.Response {
	t.Helper()
	return ssoRoundTrip(t, c, s.url+ssoLoginStart, url.Values{})
}

// passwordSession returns a browser signed in with the test password, and
// the CSRF token of its session.
func (s ssoPanel) passwordSession(t *testing.T, email string, userID int64) (*http.Client, string) {
	t.Helper()
	c := newBrowser(t)
	resp, body := tfPostForm(t, c, s.url+tfLoginPath, loginForm(email))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
	return c, latestCSRF(t, s.raw, userID)
}

func (s ssoPanel) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, s.raw.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (s ssoPanel) connections(t *testing.T, userID int64) int {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM control_panel_user_connections WHERE control_panel_user_id = $1`, userID)
}

func (s ssoPanel) audits(t *testing.T, action string, userID int64) int {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM platform_audit_log WHERE action = $1 AND actor_user_id = $2`, action, userID)
}

func (s ssoPanel) addConnection(t *testing.T, userID int64, provider, subject string) {
	t.Helper()
	_, err := s.raw.Exec(context.Background(),
		`INSERT INTO control_panel_user_connections (control_panel_user_id, provider, subject)
		 VALUES ($1, $2, $3)`, userID, provider, subject)
	require.NoError(t, err)
}

// createSSOOnlyUser makes a verified user with no password.
func (s ssoPanel) createSSOOnlyUser(t *testing.T, email, subject string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, s.raw.QueryRow(context.Background(),
		`INSERT INTO control_panel_users (email, email_verified_at) VALUES ($1, now()) RETURNING id`, email).Scan(&id))
	s.addConnection(t, id, "google", subject)
	return id
}

func (s ssoPanel) createInvite(t *testing.T, inviterID int64, email, code string) {
	t.Helper()
	_, err := s.raw.Exec(context.Background(),
		`INSERT INTO control_panel_invitations (email, role, code_hash, expires_at, invited_by_user_id)
		 VALUES ($1, 'platform_admin', $2, $3, $4)`,
		email, verifycode.Hash(nil, code), time.Now().Add(time.Hour), inviterID)
	require.NoError(t, err)
}

func (s ssoPanel) inviteAccepted(t *testing.T, email string) bool {
	t.Helper()
	return s.count(t, `SELECT count(*) FROM control_panel_invitations WHERE email = $1 AND accepted_at IS NOT NULL`, email) == 1
}

// acceptForm primes the double-submit CSRF cookie on the accept page and
// returns the form for the provider start.
func acceptForm(t *testing.T, c *http.Client, pageURL, code string) url.Values {
	t.Helper()
	resp, body := browserGet(t, c, pageURL+"?code="+url.QueryEscape(code))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	ck := jarCookie(t, c, pageURL, webutil.CSRFCookieName)
	require.NotNil(t, ck)
	return url.Values{"_csrf": {ck.Value}, "code": {code}}
}

func TestControlPanelSSO_signin(t *testing.T) {
	s := startSSOPanel(t)

	t.Run("should_reject_provider_account_with_no_link", func(t *testing.T) {
		s.setGoogleUser("g-stranger", "stranger@example.com")
		c := newBrowser(t)

		resp := s.signIn(t, c)

		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, tfLoginPath+"?sso=no_account", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM control_panel_users`))
	})

	t.Run("should_show_the_notice_on_the_login_page", func(t *testing.T) {
		resp, body := browserGet(t, newBrowser(t), s.url+tfLoginPath+"?sso=no_account")

		assert.Contains(t, body, "No control panel account is linked to this provider account.")
		assert.Contains(t, body, "Sign in with Google")
		assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "form-action 'self' "+s.fake.Server.URL)
	})

	t.Run("should_ignore_a_notice_key_that_is_not_known", func(t *testing.T) {
		_, body := browserGet(t, newBrowser(t), s.url+tfLoginPath+"?sso=%3Cb%3Eforged%3C%2Fb%3E")

		assert.NotContains(t, body, "forged")
	})

	t.Run("should_sign_in_to_the_linked_user", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "linked@example.com", "g-linked")
		s.setGoogleUser("g-linked", "address-at-google@example.com")
		c := newBrowser(t)

		resp := s.signIn(t, c)

		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, pathControlPanel, resp.Header.Get("Location"))
		assert.NotNil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM platform_audit_log
			WHERE action = 'control_panel.login' AND actor_user_id = $1 AND payload->>'method' = 'google'`, userID))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM control_panel_user_connections
			WHERE subject = 'g-linked' AND last_used_at IS NOT NULL`))
	})

	t.Run("should_reject_password_login_for_user_without_password", func(t *testing.T) {
		c := newBrowser(t)

		resp, _ := tfPostForm(t, c, s.url+tfLoginPath, loginForm("linked@example.com"))

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
	})

	t.Run("should_answer_a_disabled_user_like_an_unknown_link", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "disabled@example.com", "g-disabled")
		_, err := s.raw.Exec(context.Background(), `UPDATE control_panel_users SET disabled_at = now() WHERE id = $1`, userID)
		require.NoError(t, err)
		s.setGoogleUser("g-disabled", "disabled@example.com")
		c := newBrowser(t)

		resp := s.signIn(t, c)

		assert.Equal(t, tfLoginPath+"?sso=no_account", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
	})

	t.Run("should_send_user_with_unverified_email_to_verify", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "unverified@example.com", "g-unverified")
		_, err := s.raw.Exec(context.Background(), `UPDATE control_panel_users SET email_verified_at = NULL WHERE id = $1`, userID)
		require.NoError(t, err)
		s.setGoogleUser("g-unverified", "unverified@example.com")
		c := newBrowser(t)

		resp := s.signIn(t, c)

		assert.Equal(t, pathControlPanel+"/verify", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
	})

	t.Run("should_require_the_two_factor_code", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "tf@example.com", "g-tf")
		_, err := s.raw.Exec(context.Background(),
			`INSERT INTO control_panel_user_totp (control_panel_user_id, secret_enc, confirmed_at)
			 VALUES ($1, '\x00'::bytea, now())`, userID)
		require.NoError(t, err)
		s.setGoogleUser("g-tf", "tf@example.com")
		c := newBrowser(t)

		resp := s.signIn(t, c)

		assert.Equal(t, tfChallenge, resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
	})

	t.Run("should_report_cancelled_consent", func(t *testing.T) {
		s.fake.SetDeny(true)
		defer s.fake.SetDeny(false)

		resp := s.signIn(t, newBrowser(t))

		assert.Equal(t, tfLoginPath+"?sso=denied", resp.Header.Get("Location"))
	})
}

func TestControlPanelSSO_callback_state(t *testing.T) {
	s := startSSOPanel(t)
	s.createSSOOnlyUser(t, "state@example.com", "g-state")
	s.setGoogleUser("g-state", "state@example.com")

	t.Run("should_reject_callback_without_state_cookie", func(t *testing.T) {
		resp, _ := browserGet(t, newBrowser(t), s.url+ssoCallbackPath+"/google/callback?state=x&code=y")

		assert.Equal(t, tfLoginPath+"?sso=state", resp.Header.Get("Location"))
	})

	t.Run("should_reject_state_cookie_of_the_player_surface", func(t *testing.T) {
		// Same server key, so the signature verifies. The purpose tag is
		// what keeps a player-site state out of the control panel.
		playerFlow := &sso.Flow{
			Providers:  s.providers,
			Key:        sso.StateKey([]byte(testEmailVerifySigningKey)),
			Purpose:    "player-sso",
			CookieName: ssoStateCookie,
			CookiePath: ssoCallbackPath,
			Now:        time.Now,
		}
		rec := httptest.NewRecorder()
		require.NoError(t, playerFlow.Start(rec, httptest.NewRequest(http.MethodPost, "/", nil), "google", sso.ModeSignIn, "", ""))
		c := newBrowser(t)
		resp, _ := browserGet(t, c, rec.Header().Get("Location"))
		req, err := http.NewRequest(http.MethodGet, resp.Header.Get("Location"), nil)
		require.NoError(t, err)
		req.AddCookie(rec.Result().Cookies()[0])

		resp, err = c.Do(req)

		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, tfLoginPath+"?sso=state", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
	})

	t.Run("should_reject_replay_of_a_used_callback", func(t *testing.T) {
		c := newBrowser(t)
		resp, _ := tfPostForm(t, c, s.url+ssoLoginStart, url.Values{})
		resp, _ = browserGet(t, c, resp.Header.Get("Location"))
		callback := resp.Header.Get("Location")
		first, _ := browserGet(t, c, callback)
		require.Equal(t, pathControlPanel, first.Header.Get("Location"))

		second, _ := browserGet(t, newBrowser(t), callback)

		assert.Equal(t, tfLoginPath+"?sso=state", second.Header.Get("Location"))
	})

	t.Run("should_return_404_for_a_provider_that_is_off", func(t *testing.T) {
		c := newBrowser(t)

		start, _ := tfPostForm(t, c, s.url+tfLoginPath+"/sso/steam/start", url.Values{})
		callback, _ := browserGet(t, c, s.url+ssoCallbackPath+"/steam/callback?state=x&code=y")

		assert.Equal(t, http.StatusNotFound, start.StatusCode)
		assert.Equal(t, http.StatusNotFound, callback.StatusCode)
	})
}

func TestControlPanelSSO_invite(t *testing.T) {
	s := startSSOPanel(t)
	inviterID := createVerifiedUser(t, s.raw, "inviter@example.com")
	startURL := s.url + inviteAcceptPath + "/sso/google/start"

	t.Run("should_create_user_with_the_invite_email", func(t *testing.T) {
		s.createInvite(t, inviterID, "newhire@example.com", "code-newhire")
		s.setGoogleUser("g-newhire", "personal-address@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+inviteAcceptPath, "code-newhire"))

		require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
		assert.NotNil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM control_panel_users
			WHERE email = 'newhire@example.com' AND password_hash IS NULL
			  AND email_verified_at IS NOT NULL AND is_platform_admin`))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM control_panel_users WHERE email = 'personal-address@example.com'`))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM control_panel_user_connections WHERE subject = 'g-newhire'`))
		assert.True(t, s.inviteAccepted(t, "newhire@example.com"))
	})

	t.Run("should_accept_for_a_user_with_the_identity_linked", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "member@example.com")
		s.addConnection(t, userID, "google", "g-member")
		s.createInvite(t, inviterID, "member@example.com", "code-member")
		s.setGoogleUser("g-member", "member@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+inviteAcceptPath, "code-member"))

		require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
		assert.True(t, s.inviteAccepted(t, "member@example.com"))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM control_panel_users WHERE id = $1 AND is_platform_admin`, userID))
	})

	t.Run("should_reject_for_a_user_without_the_identity_linked", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "target@example.com")
		s.createInvite(t, inviterID, "target@example.com", "code-target")
		// A person with access to the inbox uses a provider account of
		// their own, with the same email shown by the provider.
		s.setGoogleUser("g-intruder", "target@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+inviteAcceptPath, "code-target"))

		assert.Equal(t, inviteAcceptPath+"?code=code-target&sso=accept_not_linked", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.False(t, s.inviteAccepted(t, "target@example.com"))
		assert.Equal(t, 0, s.connections(t, userID))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM control_panel_users WHERE id = $1 AND is_platform_admin`, userID))
	})

	t.Run("should_reject_identity_of_a_different_user", func(t *testing.T) {
		s.createInvite(t, inviterID, "fresh@example.com", "code-fresh")
		s.setGoogleUser("g-member", "member@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+inviteAcceptPath, "code-fresh"))

		assert.Equal(t, inviteAcceptPath+"?code=code-fresh&sso=subject_taken", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM control_panel_users WHERE email = 'fresh@example.com'`))
		assert.False(t, s.inviteAccepted(t, "fresh@example.com"))
	})

	t.Run("should_show_the_notice_with_the_form", func(t *testing.T) {
		_, body := browserGet(t, newBrowser(t), s.url+inviteAcceptPath+"?code=code-target&sso=accept_not_linked")

		assert.Contains(t, body, "Enter your password to continue.")
		assert.Contains(t, body, `name="password"`)
		assert.Contains(t, body, "Accept with Google")
	})

	t.Run("should_not_start_for_a_dead_invite_code", func(t *testing.T) {
		c := newBrowser(t)
		form := acceptForm(t, c, s.url+inviteAcceptPath, "code-target")
		form.Set("code", "no-such-code")

		resp, _ := tfPostForm(t, c, startURL, form)

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("should_reject_start_without_csrf_token", func(t *testing.T) {
		c := newBrowser(t)
		acceptForm(t, c, s.url+inviteAcceptPath, "code-target")

		resp, _ := tfPostForm(t, c, startURL, url.Values{"code": {"code-target"}})

		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestControlPanelSSO_signup_accept(t *testing.T) {
	s := startSSOPanel(t)
	startURL := s.url + signupAcceptPath + "/sso/google/start"
	createRequest := func(t *testing.T, email, tenantName, code string) {
		t.Helper()
		_, err := s.raw.Exec(context.Background(),
			`INSERT INTO tenant_signup_requests
			   (email, requested_tenant_name, final_tenant_name, project_description, status, code_hash, code_expires_at)
			 VALUES ($1, $2, $2, 'a game', 'approved', $3, $4)`,
			email, tenantName, verifycode.Hash(nil, code), time.Now().Add(time.Hour))
		require.NoError(t, err)
	}

	t.Run("should_create_user_and_tenant", func(t *testing.T) {
		createRequest(t, "studio@example.com", "Studio One", "code-studio")
		s.setGoogleUser("g-studio", "personal@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+signupAcceptPath, "code-studio"))

		require.Equal(t, pathControlPanel, resp.Header.Get("Location"))
		assert.NotNil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM control_panel_users
			WHERE email = 'studio@example.com' AND password_hash IS NULL AND NOT is_platform_admin`))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM tenants WHERE name = 'Studio One'`))
		assert.Equal(t, 1, s.count(t, `SELECT count(*) FROM tenant_signup_requests WHERE email = 'studio@example.com' AND status = 'accepted'`))
	})

	t.Run("should_reject_for_a_user_without_the_identity_linked", func(t *testing.T) {
		createVerifiedUser(t, s.raw, "owner@example.com")
		createRequest(t, "owner@example.com", "Studio Two", "code-owner")
		s.setGoogleUser("g-intruder", "owner@example.com")
		c := newBrowser(t)

		resp := ssoRoundTrip(t, c, startURL, acceptForm(t, c, s.url+signupAcceptPath, "code-owner"))

		assert.Equal(t, signupAcceptPath+"?code=code-owner&sso=accept_not_linked", resp.Header.Get("Location"))
		assert.Nil(t, jarCookie(t, c, s.url, sessionCookieName))
		assert.Equal(t, 0, s.count(t, `SELECT count(*) FROM tenants WHERE name = 'Studio Two'`))
	})
}

func TestControlPanelSSO_link(t *testing.T) {
	s := startSSOPanel(t)
	linkURL := s.url + accountSSOPath + "/google/link"

	t.Run("should_reject_link_without_session", func(t *testing.T) {
		resp, _ := tfPostForm(t, newBrowser(t), linkURL, url.Values{"current_password": {tfTestPassword}})

		assert.NotEqual(t, http.StatusOK, resp.StatusCode)
		assert.NotContains(t, resp.Header.Get("Location"), s.fake.Server.URL)
	})

	t.Run("should_reject_link_with_wrong_password", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "wrongpw@example.com")
		c, csrf := s.passwordSession(t, "wrongpw@example.com", userID)

		resp, body := tfPostForm(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {"not-the-password"}})

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, body, "Current password is incorrect")
	})

	var linkerID int64
	t.Run("should_link_provider_to_the_signed_in_user", func(t *testing.T) {
		linkerID = createVerifiedUser(t, s.raw, "linker@example.com")
		s.setGoogleUser("g-linker", "other-address@example.com")
		c, csrf := s.passwordSession(t, "linker@example.com", linkerID)

		resp := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Equal(t, pathControlPanelAccount+"?sso=linked", resp.Header.Get("Location"))
		assert.Equal(t, 1, s.connections(t, linkerID))
		assert.Equal(t, 1, s.audits(t, "control_panel.sso_link", linkerID))
	})

	t.Run("should_show_the_linked_provider_on_the_account_page", func(t *testing.T) {
		c, _ := s.passwordSession(t, "linker@example.com", linkerID)

		_, body := browserGet(t, c, s.url+pathControlPanelAccount+"?sso=linked")

		assert.Contains(t, body, "Provider account linked.")
		assert.Contains(t, body, accountSSOPath+"/google/unlink")
		assert.Contains(t, body, accountSSOPath+"/github/link")
	})

	t.Run("should_reject_subject_of_a_different_user_with_audit_row", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "intruder@example.com")
		s.setGoogleUser("g-linker", "other-address@example.com")
		c, csrf := s.passwordSession(t, "intruder@example.com", userID)

		resp := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Equal(t, pathControlPanelAccount+"?sso=subject_taken", resp.Header.Get("Location"))
		assert.Equal(t, 0, s.connections(t, userID))
		assert.Equal(t, 1, s.audits(t, "control_panel.sso_link_collision", userID))
	})

	t.Run("should_reject_second_account_of_the_same_provider", func(t *testing.T) {
		s.setGoogleUser("g-linker-second", "second@example.com")
		c, csrf := s.passwordSession(t, "linker@example.com", linkerID)

		resp := ssoRoundTrip(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		assert.Equal(t, pathControlPanelAccount+"?sso=provider_taken", resp.Header.Get("Location"))
		assert.Equal(t, 1, s.connections(t, linkerID))
	})

	t.Run("should_reject_callback_in_the_session_of_a_different_user", func(t *testing.T) {
		starterID := createVerifiedUser(t, s.raw, "starter@example.com")
		swappedID := createVerifiedUser(t, s.raw, "swapped@example.com")
		s.setGoogleUser("g-swap", "swap@example.com")
		c, csrf := s.passwordSession(t, "starter@example.com", starterID)
		resp, _ := tfPostForm(t, c, linkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})
		resp, _ = browserGet(t, c, resp.Header.Get("Location"))
		callback := resp.Header.Get("Location")
		// The same browser signs in as a different user before the
		// provider sends it back.
		resp, _ = tfPostForm(t, c, s.url+tfLoginPath, loginForm("swapped@example.com"))
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)

		resp, _ = browserGet(t, c, callback)

		assert.Equal(t, tfLoginPath+"?sso=link_session", resp.Header.Get("Location"))
		assert.Equal(t, 0, s.connections(t, starterID))
		assert.Equal(t, 0, s.connections(t, swappedID))
	})
}

func TestControlPanelSSO_unlink(t *testing.T) {
	s := startSSOPanel(t)
	unlinkURL := s.url + accountSSOPath + "/google/unlink"

	t.Run("should_unlink_when_a_password_remains", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "haspw@example.com")
		s.addConnection(t, userID, "google", "g-haspw")
		c, csrf := s.passwordSession(t, "haspw@example.com", userID)

		resp, body := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}, "current_password": {tfTestPassword}})

		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Contains(t, body, "Google unlinked.")
		assert.Equal(t, 0, s.connections(t, userID))
		assert.Equal(t, 1, s.audits(t, "control_panel.sso_unlink", userID))
	})

	t.Run("should_reject_unlink_with_wrong_password", func(t *testing.T) {
		userID := createVerifiedUser(t, s.raw, "guarded@example.com")
		s.addConnection(t, userID, "google", "g-guarded")
		c, csrf := s.passwordSession(t, "guarded@example.com", userID)

		resp, _ := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {csrf}, "current_password": {"not-the-password"}})

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, 1, s.connections(t, userID))
	})

	t.Run("should_reject_unlink_of_the_last_sign_in_method", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "only@example.com", "g-only")
		s.setGoogleUser("g-only", "only@example.com")
		c := newBrowser(t)
		s.signIn(t, c)

		resp, body := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {latestCSRF(t, s.raw, userID)}})

		assert.Equal(t, http.StatusConflict, resp.StatusCode)
		assert.Contains(t, body, "only sign-in method")
		assert.Equal(t, 1, s.connections(t, userID))
	})

	t.Run("should_unlink_when_a_second_connection_remains", func(t *testing.T) {
		userID := s.createSSOOnlyUser(t, "two@example.com", "g-two")
		s.addConnection(t, userID, "github", "gh-two")
		s.setGoogleUser("g-two", "two@example.com")
		c := newBrowser(t)
		s.signIn(t, c)

		resp, body := tfPostForm(t, c, unlinkURL, url.Values{"_csrf": {latestCSRF(t, s.raw, userID)}})

		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		assert.Equal(t, 1, s.connections(t, userID))
	})

	t.Run("should_show_set_password_link_in_place_of_the_change_form", func(t *testing.T) {
		s.setGoogleUser("g-only", "only@example.com")
		c := newBrowser(t)
		s.signIn(t, c)

		_, body := browserGet(t, c, s.url+pathControlPanelAccount)

		assert.Contains(t, body, "No password is set.")
		assert.NotContains(t, body, `name="new_password"`)
	})
}
