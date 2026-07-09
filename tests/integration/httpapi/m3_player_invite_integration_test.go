//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/players"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

// newControlPanelAndPlayerServer wires the control panel AND the player UI
// together so the cross-stack player-invite test can simulate the full
// invite-accept flow.
func newControlPanelAndPlayerServer(t *testing.T, c *cluster) (*httptest.Server, *mailer.Recorder) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	rec := &mailer.Recorder{}
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	router := httpapi.NewRouter(httpapi.Deps{
		Version:  "v1",
		Commit:   "test",
		Pool:     pool,
		Lookup:   tenant.NewSQLLookup(c.appPool),
		Limiter:  ratelimit.NewCacheLimiter(c.cache),
		Signer:   signer,
		Cache:    c.cache,
		Mailer:   rec,
		MailFrom: "no-reply@example.test",
		RBAC:     authorizer,
		ControlPanel: controlpanel.Config{
			Mount:    true,
			BaseURL:  "http://app.example.test",
			MailFrom: "no-reply@example.test",
		},
		ControlPanelBootstrap: controlpanel.DisabledBootstrap(),
		Players:               players.Config{Mount: true},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestPlayerInvite_rejects_project_belonging_to_other_tenant covers H2:
// a tenant-admin must not be able to create a player invite against a
// project that belongs to a DIFFERENT tenant by crafting the URL.
func TestPlayerInvite_rejects_project_belonging_to_other_tenant(t *testing.T) {
	c := startCluster(t)

	// Tenant A with project A — actor is admin here.
	tenantA, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "key-a")
	// Tenant B with project B — actor must NOT be able to invite into.
	_, projectB := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "key-b")

	adminID := seedControlPanelUser(t, c, "admin-a@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")

	srv, _ := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin-a@example.com", "correct-horse-battery-staple")

	form := url.Values{"_csrf": {csrf}, "email": {"player@example.com"}}
	target := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectB, 10) + "/players/invite"

	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Pre-fix would have returned 303 redirect to "/players?flash=Invite sent…".
	// Post-fix it surfaces a 404 with the "project does not belong to this tenant" copy.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "does not belong")

	// And no row was inserted.
	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM player_invitations WHERE email = 'player@example.com'`).Scan(&count))
	assert.Equal(t, int64(0), count)
}

// TestPlayerInvite_happy_path_creates_account_and_logs_in covers H6:
// control panel admin invites a player; the magic-link URL in the email
// actually resolves (was a 404 pre-fix); accepting it creates the
// player, marks them verified, and issues a player session.
func TestPlayerInvite_happy_path_creates_account_and_logs_in(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	// 1) Admin sends the invite.
	form := url.Values{"_csrf": {csrf}, "email": {"newplayer@example.com"}}
	invitePath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players/invite"
	req, err := http.NewRequest(http.MethodPost, invitePath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	// 2) Extract the magic-link URL from the email body.
	body := rec.Sent[0].Body
	const marker = "/v1/players/p/"
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "email body should contain the player invite URL: %q", body)
	rest := body[i:]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	linkPath := rest[:end]
	// The link is encoded as full URL; trim the scheme/host so we hit our test server.
	if idx := strings.Index(linkPath, marker); idx > 0 {
		linkPath = linkPath[idx:]
	}

	// 3) GET the invite-accept page (used to be 404 pre-fix). The same
	// request sets the CSRF cookie; harvest it for the POST below.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	getClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getResp, err := getClient.Get(srv.URL + linkPath)
	require.NoError(t, err)
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, string(getBody))
	assert.Contains(t, string(getBody), "newplayer@example.com")

	// Pull the CSRF token out of the rendered hidden field.
	csrfToken := extractCSRFFromForm(t, string(getBody))

	// 4) POST the password to accept (with CSRF cookie + field).
	codeParam, err := url.ParseQuery(strings.SplitN(linkPath, "?", 2)[1])
	require.NoError(t, err)
	require.NotEmpty(t, codeParam.Get("code"))

	acceptForm := url.Values{
		"_csrf":    {csrfToken},
		"code":     {codeParam.Get("code")},
		"password": {"playerpass1"},
	}
	acceptPath := strings.SplitN(linkPath, "?", 2)[0]
	acceptReq, err := http.NewRequest(http.MethodPost, srv.URL+acceptPath,
		strings.NewReader(acceptForm.Encode()))
	require.NoError(t, err)
	acceptReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acceptClient := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	acceptResp, err := acceptClient.Do(acceptReq)
	require.NoError(t, err)
	acceptResp.Body.Close()
	require.Equal(t, http.StatusSeeOther, acceptResp.StatusCode)

	// 5) Player row exists and is verified.
	var playerID int64
	var verifiedAt, disabledAt *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id, email_verified_at::text, disabled_at::text FROM project_players WHERE email = 'newplayer@example.com'`).
		Scan(&playerID, &verifiedAt, &disabledAt))
	assert.Greater(t, playerID, int64(0))
	assert.NotNil(t, verifiedAt)
	assert.Nil(t, disabledAt)

	// 6) Invitation row marked accepted.
	var accepted *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT accepted_at::text FROM player_invitations WHERE email = 'newplayer@example.com'`).Scan(&accepted))
	assert.NotNil(t, accepted)

	// 7) Global account session cookie was issued (invites now sign the
	// invitee into their gg-scale account, not a per-project session).
	var sawSession bool
	for _, ck := range acceptResp.Cookies() {
		if ck.Name == "ggscale_account_session" {
			sawSession = true
		}
	}
	assert.True(t, sawSession, "expected ggscale_account_session cookie after accept")

	// 8) The player is linked to a verified global account.
	var linkedAccount *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT player_account_id::text FROM project_players WHERE email = 'newplayer@example.com'`).Scan(&linkedAccount))
	require.NotNil(t, linkedAccount)
	var acctVerified *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email_verified_at::text FROM player_accounts WHERE email = 'newplayer@example.com'`).Scan(&acctVerified))
	assert.NotNil(t, acctVerified)
}

// csrfHiddenFieldRE matches the rendered `<input ... name="_csrf" value="…">`
// element in any of the player/control-panel templates. Used by integration
// tests that drive form posts and need to round-trip the CSRF token.
var csrfHiddenFieldRE = regexp.MustCompile(`name="_csrf"\s+value="([^"]+)"`)

func extractCSRFFromForm(t *testing.T, body string) string {
	t.Helper()
	m := csrfHiddenFieldRE.FindStringSubmatch(body)
	require.Len(t, m, 2, "expected _csrf hidden input in body")
	return m[1]
}
