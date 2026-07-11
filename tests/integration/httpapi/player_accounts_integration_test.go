//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

var (
	csrfFieldRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)
	sixDigitRe  = regexp.MustCompile(`\b(\d{6})\b`)
)

// getPlayerCSRF GETs a player-site page and returns its double-submit CSRF
// token (the cookie is stored in the client's jar).
func getPlayerCSRF(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	m := csrfFieldRe.FindStringSubmatch(string(body))
	require.Len(t, m, 2, "csrf token not found in page")
	return m[1]
}

func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestPlayerAccount_signup_verify_login_happy_path drives the full global
// player-account flow through the server-rendered player site.
func TestPlayerAccount_signup_verify_login_happy_path(t *testing.T) {
	c := startCluster(t)
	srv, rec := newControlPanelAndPlayerServer(t, c)
	client := jarClient(t)

	base := srv.URL + "/v1/players/account"
	const email = "player-one@example.com"
	const password = "correct-horse-battery-staple"

	// Signup.
	csrf := getPlayerCSRF(t, client, base+"/signup")
	resp, err := client.PostForm(base+"/signup", url.Values{
		"_csrf": {csrf}, "email": {email}, "password": {password}, "display_name": {"Player One"},
	})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/players/account/verify", resp.Header.Get("Location"))

	// Recover the 6-digit code from the verification email.
	require.NotEmpty(t, rec.Sent)
	codeMatch := sixDigitRe.FindStringSubmatch(rec.Sent[len(rec.Sent)-1].Body)
	require.Len(t, codeMatch, 2, "no verification code in email body")
	code := codeMatch[1]

	// Verify.
	csrf = getPlayerCSRF(t, client, base+"/verify")
	resp, err = client.PostForm(base+"/verify", url.Values{"_csrf": {csrf}, "code": {code}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/players/account/", resp.Header.Get("Location"))

	// Account home renders with the email.
	homeResp, err := client.Get(base + "/")
	require.NoError(t, err)
	defer homeResp.Body.Close()
	body, _ := io.ReadAll(homeResp.Body)
	require.Equal(t, http.StatusOK, homeResp.StatusCode, string(body))
	assert.Contains(t, string(body), email)

	// The account is verified in the DB.
	var verified bool
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email_verified_at IS NOT NULL FROM player_accounts WHERE email = $1`, email).Scan(&verified))
	assert.True(t, verified)

	// Logout then log back in (proves the password path, not just verify).
	csrf = getPlayerCSRF(t, client, base+"/")
	resp, err = client.PostForm(base+"/logout", url.Values{"_csrf": {csrf}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	loginClient := jarClient(t)
	csrf = getPlayerCSRF(t, loginClient, base+"/login")
	resp, err = loginClient.PostForm(base+"/login", url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/players/account/", resp.Header.Get("Location"))
}

// TestPlayerAccount_disabled_account_rejected proves a platform-disabled
// account cannot log in.
func TestPlayerAccount_disabled_account_rejected(t *testing.T) {
	c := startCluster(t)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	client := jarClient(t)
	base := srv.URL + "/v1/players/account"
	const email = "disabled-player@example.com"
	const password = "correct-horse-battery-staple"

	seedDisabledPlayerAccount(t, c, email, password)

	csrf := getPlayerCSRF(t, client, base+"/login")
	resp, err := client.PostForm(base+"/login", url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestPlayerAccount_epoch_bump_revokes_session proves the account-level
// revocation lever: bumping session_epoch invalidates a live account session.
func TestPlayerAccount_epoch_bump_revokes_session(t *testing.T) {
	c := startCluster(t)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	client := jarClient(t)
	base := srv.URL + "/v1/players/account"
	const email = "epoch-player@example.com"
	const password = "correct-horse-battery-staple"

	seedVerifiedPlayerAccount(t, c, email, password)

	csrf := getPlayerCSRF(t, client, base+"/login")
	resp, err := client.PostForm(base+"/login", url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Session works before the bump.
	pre, err := client.Get(base + "/")
	require.NoError(t, err)
	pre.Body.Close()
	require.Equal(t, http.StatusOK, pre.StatusCode)

	// Bump the epoch (as a password change / disable would).
	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE player_accounts SET session_epoch = session_epoch + 1 WHERE email = $1`, email)
	require.NoError(t, err)

	// The stale-epoch session is now rejected → redirect to login.
	post, err := client.Get(base + "/")
	require.NoError(t, err)
	defer post.Body.Close()
	assert.Equal(t, http.StatusSeeOther, post.StatusCode)
	assert.Equal(t, "/v1/players/account/login", post.Header.Get("Location"))
}

// TestPlayerAccount_direct_join_route_is_not_available proves players cannot
// link themselves into a project by posting a project ID. Projects must link
// players through invite acceptance or authenticated project/API-key flows.
func TestPlayerAccount_direct_join_route_is_not_available(t *testing.T) {
	c := startCluster(t)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	base := srv.URL + "/v1/players/account"
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "join-key")

	const email = "joiner@example.com"
	seedVerifiedPlayerAccount(t, c, email, "correct-horse-battery-staple")
	client := loginPlayerAccount(t, base, email, "correct-horse-battery-staple")

	csrf := getPlayerCSRF(t, client, base+"/")
	resp, err := client.PostForm(base+"/join", url.Values{"_csrf": {csrf}, "project_id": {"1"}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	var linked int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players e
		 JOIN player_accounts a ON a.id = e.player_account_id
		 WHERE e.project_id = $1 AND a.email = $2`, projectID, email).Scan(&linked))
	assert.Equal(t, 0, linked, "direct player join route must not create a link")
}

func loginPlayerAccount(t *testing.T, base, email, password string) *http.Client {
	t.Helper()
	client := jarClient(t)
	csrf := getPlayerCSRF(t, client, base+"/login")
	resp, err := client.PostForm(base+"/login", url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	return client
}

func seedDisabledPlayerAccount(t *testing.T, c *cluster, email, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at, disabled_at)
		 VALUES ($1, $2, now(), now())`, email, hash)
	require.NoError(t, err)
}

func seedVerifiedPlayerAccount(t *testing.T, c *cluster, email, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ($1, $2, now())`, email, hash)
	require.NoError(t, err)
}
