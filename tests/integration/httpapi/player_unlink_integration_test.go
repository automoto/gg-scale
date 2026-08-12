//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/mailer"
)

// sendPlayerInvite drives the control-panel player-invite form and returns the
// invite-accept link path from the recorded email.
func sendPlayerInvite(t *testing.T, srv *httptest.Server, rec *mailer.Recorder, cookie *http.Cookie, csrf string, tenantID, projectID int64, email string) string {
	t.Helper()
	before := len(rec.Sent)
	form := url.Values{"_csrf": {csrf}, "email": {email}}
	invitePath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/players/invite"
	req, err := http.NewRequest(http.MethodPost, invitePath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, before+1)

	body := rec.Sent[before].Body
	const marker = "/v1/players/p/"
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "email body should contain the player invite URL: %q", body)
	rest := body[i:]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// acceptPlayerInvite GETs the invite page and POSTs the accept form. password
// may be empty when the invitee already has a gg-scale account.
func acceptPlayerInvite(t *testing.T, srv *httptest.Server, linkPath, password string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	getResp, err := client.Get(srv.URL + linkPath)
	require.NoError(t, err)
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, string(getBody))
	csrfToken := extractCSRFFromForm(t, string(getBody))

	codeParam, err := url.ParseQuery(strings.SplitN(linkPath, "?", 2)[1])
	require.NoError(t, err)
	form := url.Values{"_csrf": {csrfToken}, "code": {codeParam.Get("code")}}
	if password != "" {
		form.Set("password", password)
	}
	acceptPath := strings.SplitN(linkPath, "?", 2)[0]
	req, err := http.NewRequest(http.MethodPost, srv.URL+acceptPath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, string(body))
}

// playerAccountClient signs into the player site with a global account and
// returns a cookie-jar client carrying the account session.
func playerAccountClient(t *testing.T, baseURL, email, password string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(baseURL + "/v1/players/account/login")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	csrf := extractCSRFFromForm(t, string(body))

	form := url.Values{"_csrf": {csrf}, "email": {email}, "password": {password}}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/players/account/login", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := client.Do(req)
	require.NoError(t, err)
	loginBody, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, loginResp.StatusCode, string(loginBody))
	return client
}

func getPage(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := client.Get(target)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func postAccountForm(t *testing.T, client *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

func TestPlayerUnlink_non_destructive_blocks_auth_and_reinvite_restores(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	// Allow-all limiter: the flow makes many back-to-back auth calls and
	// per-IP throttling is not part of its assertions.
	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	// Invite + accept: creates the linked player and the global account.
	link := sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "player@example.com")
	acceptPlayerInvite(t, srv, link, "accountpass1")

	var playerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT id FROM project_players WHERE email = 'player@example.com'`).Scan(&playerID))

	// Give the row a per-project password so /v1/auth/login exercises the
	// player-credential path.
	gameHash, err := bcrypt.GenerateFromPassword([]byte("gamepass12"), bcrypt.MinCost)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(ctx,
		`UPDATE project_players SET password_hash = $1 WHERE id = $2`, gameHash, playerID)
	require.NoError(t, err)

	// Baseline: game login, access token, and refresh all work.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "key-a",
		map[string]string{"email": "player@example.com", "password": "gamepass12"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var session struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(body, &session))
	resp, body = authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "key-a", session.AccessToken,
		map[string]string{"xuid": "unlink-test"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(body))

	// The account home lists the game with an unlink action.
	account := playerAccountClient(t, srv.URL, "player@example.com", "accountpass1")
	status, home := getPage(t, account, srv.URL+"/v1/players/account/")
	require.Equal(t, http.StatusOK, status)
	unlinkPath := "/v1/players/account/projects/" + strconv.FormatInt(playerID, 10) + "/unlink"
	assert.Contains(t, home, unlinkPath)

	// Confirm page renders before anything changes.
	status, confirm := getPage(t, account, srv.URL+unlinkPath)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, confirm, "Unlink")
	accountCSRF := extractCSRFFromForm(t, confirm)

	// A foreign player id is not unlinkable — 404, nothing leaks.
	status, _ = postAccountForm(t, account, srv.URL+"/v1/players/account/projects/999999/unlink",
		url.Values{"_csrf": {accountCSRF}})
	assert.Equal(t, http.StatusNotFound, status)

	// Unlink.
	status, _ = postAccountForm(t, account, srv.URL+unlinkPath, url.Values{"_csrf": {accountCSRF}})
	require.Equal(t, http.StatusSeeOther, status)

	// Non-destructive: same row, data kept, link inactive.
	var email string
	var accountRef *string
	var unlinkedAt *string
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT email, player_account_id::text, unlinked_at::text FROM project_players WHERE id = $1`,
		playerID).Scan(&email, &accountRef, &unlinkedAt))
	assert.Equal(t, "player@example.com", email)
	assert.Nil(t, accountRef, "unlink must clear the account link")
	assert.NotNil(t, unlinkedAt, "unlink must mark the link inactive")

	// The account home no longer lists the game.
	status, home = getPage(t, account, srv.URL+"/v1/players/account/")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, home, unlinkPath)

	// Player traffic is blocked: login, old access token, and refresh fail.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "key-a",
		map[string]string{"email": "player@example.com", "password": "gamepass12"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPatch, srv.URL+"/v1/profile", "key-a", session.AccessToken,
		map[string]string{"xuid": "unlink-test-2"})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/refresh", "key-a",
		map[string]string{"refresh_token": session.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))

	// Re-invite restores the same row: the invitee already has an account, so
	// no password is needed on accept.
	link = sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "player@example.com")
	acceptPlayerInvite(t, srv, link, "")

	var restoredID int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT id FROM project_players WHERE email = 'player@example.com' AND deleted_at IS NULL`).Scan(&restoredID))
	assert.Equal(t, playerID, restoredID, "re-invite must restore the same project_players row")
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT player_account_id::text, unlinked_at::text FROM project_players WHERE id = $1`,
		playerID).Scan(&accountRef, &unlinkedAt))
	assert.NotNil(t, accountRef, "re-invite must restore the account link")
	assert.Nil(t, unlinkedAt, "re-invite must clear the inactive marker")

	// And game login works again.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "key-a",
		map[string]string{"email": "player@example.com", "password": "gamepass12"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}
