//go:build integration

package httpapi_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/mailer"
)

// waitForSentCount blocks until the recorder holds want messages (delivery
// runs off-request) and returns the snapshot.
func waitForSentCount(t *testing.T, rec *mailer.Recorder, want int) []mailer.Message {
	t.Helper()
	var sent []mailer.Message
	require.Eventually(t, func() bool {
		sent = rec.Snapshot()
		return len(sent) >= want
	}, 5*time.Second, 20*time.Millisecond, "expected %d recorded emails", want)
	require.Len(t, sent, want)
	return sent
}

// resetLinkFromBody pulls the reset-password path (with token) out of an
// email body.
func resetLinkFromBody(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "email should contain the reset link: %q", body)
	rest := body[i:]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func tokenFromLink(t *testing.T, link string) string {
	t.Helper()
	parts := strings.SplitN(link, "token=", 2)
	require.Len(t, parts, 2)
	return parts[1]
}

func TestForgotPassword_control_panel_full_flow(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "cp-reset@example.com", "old-password-123", false)
	// Allow-all limiter: the flow makes many back-to-back login/reset calls
	// and per-IP throttling is not part of its assertions.
	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})

	// The login page offers the flow.
	resp, err := http.Get(srv.URL + "/v1/control-panel/login")
	require.NoError(t, err)
	loginBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(loginBody), "/v1/control-panel/forgot-password")

	// Establish a session that the reset must revoke.
	oldCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "cp-reset@example.com", "old-password-123")

	// Request a reset. The response never says whether the account exists.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/forgot-password",
		url.Values{"email": {"cp-reset@example.com"}})
	require.NoError(t, err)
	knownBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(knownBody), "If an account matches")
	sent := waitForSentCount(t, rec, 1)
	linkOne := resetLinkFromBody(t, sent[0].Body, "/v1/control-panel/reset-password")

	// Unknown email: identical response, no email.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/forgot-password",
		url.Values{"email": {"nobody@example.com"}})
	require.NoError(t, err)
	unknownBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, string(knownBody), string(unknownBody), "responses must not reveal account existence")
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, rec.Snapshot(), 1)

	// Oversized address (past the RFC 5321 254-byte cap): same constant
	// response, and no delivery work is started or enqueued for it.
	oversized := strings.Repeat("a", 300) + "@example.com"
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/forgot-password",
		url.Values{"email": {oversized}})
	require.NoError(t, err)
	oversizedBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, string(knownBody), string(oversizedBody))
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, rec.Snapshot(), 1, "an oversized address must not start delivery work")

	// A second request mints a second, independent link.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/forgot-password",
		url.Values{"email": {"cp-reset@example.com"}})
	require.NoError(t, err)
	resp.Body.Close()
	sent = waitForSentCount(t, rec, 2)
	linkTwo := resetLinkFromBody(t, sent[1].Body, "/v1/control-panel/reset-password")
	tokenTwo := tokenFromLink(t, linkTwo)

	// The reset form renders for a valid link.
	resp, err = http.Get(srv.URL + linkTwo)
	require.NoError(t, err)
	formBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(formBody))

	// A rejected password (over bcrypt's 72-byte limit) must not burn the link.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/reset-password",
		url.Values{"token": {tokenTwo}, "password": {strings.Repeat("x", 80)}})
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	// The same link still works with a valid password.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/reset-password",
		url.Values{"token": {tokenTwo}, "password": {"brand-new-password-1"}})
	require.NoError(t, err)
	doneBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(doneBody))

	// Old password fails, new password works.
	form := url.Values{"email": {"cp-reset@example.com"}, "password": {"old-password-123"}}
	resp, err = noRedirectClient().PostForm(srv.URL+"/v1/control-panel/login", form)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	controlPanelLoginCookieAndCSRF(t, srv.URL, "cp-reset@example.com", "brand-new-password-1")

	// The pre-reset session is dead.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel", nil)
	require.NoError(t, err)
	req.AddCookie(oldCookie)
	resp, err = noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "old session must be revoked after reset")

	// The consumed token is single-use.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/reset-password",
		url.Values{"token": {tokenTwo}, "password": {"another-password-abc"}})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusGone, resp.StatusCode, "a used token must be rejected")

	// The OTHER outstanding link died with the reset: an attacker holding an
	// older email cannot reset the new password.
	resp, err = http.PostForm(srv.URL+"/v1/control-panel/reset-password",
		url.Values{"token": {tokenFromLink(t, linkOne)}, "password": {"attacker-password-99"}})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusGone, resp.StatusCode, "outstanding links must be invalidated by a reset")
}

func TestForgotPassword_player_account_full_flow(t *testing.T) {
	c := startCluster(t)
	// Allow-all limiter: the flow makes several back-to-back login/reset
	// calls and per-IP throttling is not part of its assertions.
	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})

	seedVerifiedPlayerAccount(t, c, "player-reset@example.com", "old-account-pass1")

	// Session that the reset must invalidate.
	account := playerAccountClient(t, srv.URL, "player-reset@example.com", "old-account-pass1")

	// The login page offers the flow.
	status, loginPage := getPage(t, account, srv.URL+"/v1/players/account/login")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, loginPage, "/v1/players/account/forgot-password")

	// Request two resets (CSRF form flow): the older link must die when the
	// newer one is used.
	status, forgotPage := getPage(t, account, srv.URL+"/v1/players/account/forgot-password")
	require.Equal(t, http.StatusOK, status)
	csrf := extractCSRFFromForm(t, forgotPage)
	status, knownBody := postAccountForm(t, account, srv.URL+"/v1/players/account/forgot-password",
		url.Values{"_csrf": {csrf}, "email": {"player-reset@example.com"}})
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, knownBody, "If an account matches")
	sent := waitForSentCount(t, rec, 1)
	linkOne := resetLinkFromBody(t, sent[0].Body, "/v1/players/account/reset-password")

	// Unknown email: same response, no email.
	status, unknownBody := postAccountForm(t, account, srv.URL+"/v1/players/account/forgot-password",
		url.Values{"_csrf": {csrf}, "email": {"nobody@example.com"}})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, knownBody, unknownBody)
	time.Sleep(200 * time.Millisecond)
	assert.Len(t, rec.Snapshot(), 1)

	status, _ = postAccountForm(t, account, srv.URL+"/v1/players/account/forgot-password",
		url.Values{"_csrf": {csrf}, "email": {"player-reset@example.com"}})
	require.Equal(t, http.StatusOK, status)
	sent = waitForSentCount(t, rec, 2)
	linkTwo := resetLinkFromBody(t, sent[1].Body, "/v1/players/account/reset-password")
	tokenTwo := tokenFromLink(t, linkTwo)

	// Follow the newer link, set a new password.
	status, formPage := getPage(t, account, srv.URL+linkTwo)
	require.Equal(t, http.StatusOK, status, formPage)
	resetCSRF := extractCSRFFromForm(t, formPage)
	status, doneBody := postAccountForm(t, account, srv.URL+"/v1/players/account/reset-password",
		url.Values{"_csrf": {resetCSRF}, "token": {tokenTwo}, "password": {"new-account-pass1"}})
	require.Equal(t, http.StatusOK, status, doneBody)

	// Old session is dead: account home bounces to login.
	status, _ = getPage(t, account, srv.URL+"/v1/players/account/")
	assert.Equal(t, http.StatusSeeOther, status, "old account session must be invalidated")

	// Old password fails, new password works.
	status, _ = getPage(t, account, srv.URL+"/v1/players/account/login")
	require.Equal(t, http.StatusOK, status)
	playerAccountClient(t, srv.URL, "player-reset@example.com", "new-account-pass1")

	// The consumed token is single-use, and the older outstanding link died
	// with the reset.
	status, _ = postAccountForm(t, account, srv.URL+"/v1/players/account/reset-password",
		url.Values{"_csrf": {resetCSRF}, "token": {tokenTwo}, "password": {"yet-another-pass1"}})
	assert.Equal(t, http.StatusGone, status)
	status, _ = postAccountForm(t, account, srv.URL+"/v1/players/account/reset-password",
		url.Values{"_csrf": {resetCSRF}, "token": {tokenFromLink(t, linkOne)}, "password": {"attacker-pass-99"}})
	assert.Equal(t, http.StatusGone, status, "outstanding links must be invalidated by a reset")
}
