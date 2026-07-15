//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedExternalIDOnlyPlayer inserts a project_players row with an external ID but
// no email — the "link player" starting state (an anonymous/game-only player).
func seedExternalIDOnlyPlayer(t *testing.T, c *cluster, tenantID, projectID int64, externalID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id)
		 VALUES ($1, $2, $3) RETURNING id`, tenantID, projectID, externalID).Scan(&id))
	return id
}

// TestLinkPlayer_binds_email_onto_existing_row: an admin links an email to an
// existing external-id-only player; accepting the invite binds the proven email
// + verified account onto THAT row (no new row) and marks it verified.
func TestLinkPlayer_binds_email_onto_existing_row(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-linktest-1")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	linkPath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players/" + strconv.FormatInt(playerID, 10) + "/link"

	// 1) The dialog GET shows the read-only external ID.
	dlgReq, err := http.NewRequest(http.MethodGet, linkPath, nil)
	require.NoError(t, err)
	dlgReq.AddCookie(cookie)
	dlgResp, err := http.DefaultClient.Do(dlgReq)
	require.NoError(t, err)
	dlgBody, _ := io.ReadAll(dlgResp.Body)
	dlgResp.Body.Close()
	require.Equal(t, http.StatusOK, dlgResp.StatusCode, string(dlgBody))
	assert.Contains(t, string(dlgBody), "player-linktest-1")

	// 2) Admin submits the email.
	form := url.Values{"_csrf": {csrf}, "email": {"linked@example.com"}}
	req, err := http.NewRequest(http.MethodPost, linkPath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	// 3) The invitation targets our row.
	var targetID *int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT project_player_id FROM player_invitations WHERE email = 'linked@example.com'`).Scan(&targetID))
	require.NotNil(t, targetID)
	assert.Equal(t, playerID, *targetID)

	// 4) Accept the invite via the magic link.
	acceptViaMagicLink(t, srv.URL, rec.Sent[0].Body)

	// 5) The SAME row is now verified, emailed, and account-linked.
	var email, verifiedAt, account *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email::text, email_verified_at::text, player_account_id::text
		 FROM project_players WHERE id = $1`, playerID).Scan(&email, &verifiedAt, &account))
	require.NotNil(t, email)
	assert.Equal(t, "linked@example.com", *email)
	assert.NotNil(t, verifiedAt)
	assert.NotNil(t, account)

	// 6) No extra player row was created for that email.
	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE project_id = $1 AND email = 'linked@example.com'`, projectA).Scan(&count))
	assert.Equal(t, int64(1), count)

	// 7) Invitation marked accepted.
	var accepted *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT accepted_at::text FROM player_invitations WHERE email = 'linked@example.com'`).Scan(&accepted))
	assert.NotNil(t, accepted)
}

// TestLinkPlayer_rejects_email_owned_by_another_player: linking an email that
// already belongs to a different player in the project is a 409, no invite sent.
func TestLinkPlayer_rejects_email_owned_by_another_player(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	targetPlayer := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-linktest-2")

	// A different player already owns taken@example.com.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id, email)
		 VALUES ($1, $2, 'player-owner', 'taken@example.com')`, tenantA, projectA)
	require.NoError(t, err)

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	form := url.Values{"_csrf": {csrf}, "email": {"taken@example.com"}}
	linkPath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players/" + strconv.FormatInt(targetPlayer, 10) + "/link"
	req, err := http.NewRequest(http.MethodPost, linkPath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Empty(t, rec.Sent)
	// The error re-renders the dialog fragment (with an inline banner) rather
	// than replacing the page with a bare error string.
	assert.Contains(t, string(body), "already used by another player")
	assert.Contains(t, string(body), `name="email"`, "the Link dialog form is re-rendered, not a blank error page")

	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM player_invitations WHERE project_player_id = $1`, targetPlayer).Scan(&count))
	assert.Equal(t, int64(0), count)
}

// TestLinkPlayer_supersedes_prior_plain_open_invite: a link invite to an email
// that already has a PLAIN (untargeted) open invite must supersede it instead
// of colliding on the (project_id, email) open-invite unique index.
func TestLinkPlayer_supersedes_prior_plain_open_invite(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-plain-super")

	// A prior plain (project_player_id NULL) open invite already targets the email.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO player_invitations (tenant_id, project_id, email, code_hash, expires_at, invited_by_user_id)
		 VALUES ($1, $2, 'super@example.com', $3, now() + interval '3 days', $4)`,
		tenantA, projectA, []byte("plain-hash-super"), adminID)
	require.NoError(t, err)

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "super@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	var open, revoked int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT
		   count(*) FILTER (WHERE accepted_at IS NULL AND revoked_at IS NULL),
		   count(*) FILTER (WHERE revoked_at IS NOT NULL)
		 FROM player_invitations WHERE project_id = $1 AND email = 'super@example.com'`, projectA).Scan(&open, &revoked))
	assert.Equal(t, int64(1), open, "exactly one open invite after superseding the plain one")
	assert.Equal(t, int64(1), revoked, "the prior plain invite was superseded")

	// The surviving open invite is the targeted link invite.
	var targetID *int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT project_player_id FROM player_invitations
		 WHERE email = 'super@example.com' AND accepted_at IS NULL AND revoked_at IS NULL`).Scan(&targetID))
	require.NotNil(t, targetID)
	assert.Equal(t, playerID, *targetID)
}

// TestLinkPlayer_accept_rejected_when_row_self_verified_other_email: if the
// target row verifies a different email through the normal flow after the link
// invite is minted, accepting the invite must NOT clobber that verified email.
func TestLinkPlayer_accept_rejected_when_row_self_verified_other_email(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-selfverify")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "old@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	// Before the invite is accepted, the player verifies a DIFFERENT address.
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET email = 'new@example.com', email_verified_at = now() WHERE id = $1`, playerID)
	require.NoError(t, err)

	status, body := acceptInvite(t, srv.URL, rec.Sent[0].Body)
	assert.Equal(t, http.StatusConflict, status, body)

	// The self-verified email survives untouched.
	var email, verifiedAt *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email::text, email_verified_at::text FROM project_players WHERE id = $1`, playerID).Scan(&email, &verifiedAt))
	require.NotNil(t, email)
	assert.Equal(t, "new@example.com", *email)
	require.NotNil(t, verifiedAt)
}

// TestLinkPlayer_accept_notfound_when_row_soft_deleted: if the target row is
// soft-deleted before the invite is accepted, acceptance surfaces a not-found
// (the invite is dead) rather than a misleading "already linked" conflict.
func TestLinkPlayer_accept_notfound_when_row_soft_deleted(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-softdel")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "gone@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET deleted_at = now() WHERE id = $1`, playerID)
	require.NoError(t, err)

	status, body := acceptInvite(t, srv.URL, rec.Sent[0].Body)
	assert.Equal(t, http.StatusNotFound, status, body)
}

// TestLinkPlayer_expired_invite_hides_pending_badge: an expired-but-unswept
// link invite must not render an "invite pending" badge, since the accept flow
// would reject it.
func TestLinkPlayer_expired_invite_hides_pending_badge(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-expired")

	// An expired open invite targets the row (not yet swept).
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO player_invitations (tenant_id, project_id, email, code_hash, expires_at, invited_by_user_id, project_player_id)
		 VALUES ($1, $2, 'expired@example.com', $3, now() - interval '1 hour', $4, $5)`,
		tenantA, projectA, []byte("expired-hash"), adminID, playerID)
	require.NoError(t, err)

	srv, _ := newControlPanelAndPlayerServer(t, c)
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	listPath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players"
	req, err := http.NewRequest(http.MethodGet, listPath, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	listResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode, string(listBody))
	assert.NotContains(t, string(listBody), "invite pending",
		"an expired (unswept) invite must not render a pending badge")
}

// acceptViaMagicLink drives the accept flow and asserts a successful (303) accept.
func acceptViaMagicLink(t *testing.T, base, body string) {
	t.Helper()
	status, respBody := acceptInvite(t, base, body)
	require.Equal(t, http.StatusSeeOther, status, respBody)
}

// acceptInvite extracts the invite URL from an email body, GETs the accept page,
// and POSTs the password to accept. It returns the final accept-POST status +
// body so callers can assert either success or a conflict.
func acceptInvite(t *testing.T, base, body string) (int, string) {
	t.Helper()
	const marker = "/v1/players/p/"
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "email body should contain the invite URL: %q", body)
	rest := body[i:]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	linkPath := rest[:end]

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getResp, err := client.Get(base + linkPath)
	require.NoError(t, err)
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	require.Equal(t, http.StatusOK, getResp.StatusCode, string(getBody))
	csrfToken := extractCSRFFromForm(t, string(getBody))

	codeParam, err := url.ParseQuery(strings.SplitN(linkPath, "?", 2)[1])
	require.NoError(t, err)
	require.NotEmpty(t, codeParam.Get("code"))

	acceptForm := url.Values{
		"_csrf":    {csrfToken},
		"code":     {codeParam.Get("code")},
		"password": {"playerpass1"},
	}
	acceptPath := strings.SplitN(linkPath, "?", 2)[0]
	acceptReq, err := http.NewRequest(http.MethodPost, base+acceptPath, strings.NewReader(acceptForm.Encode()))
	require.NoError(t, err)
	acceptReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	acceptResp, err := client.Do(acceptReq)
	require.NoError(t, err)
	acceptBody, _ := io.ReadAll(acceptResp.Body)
	acceptResp.Body.Close()
	return acceptResp.StatusCode, string(acceptBody)
}

// postLink submits the link-player form for playerID and returns the response.
func postLink(t *testing.T, srvURL string, cookie *http.Cookie, csrf string, tenantID, projectID, playerID int64, email string) *http.Response {
	t.Helper()
	form := url.Values{"_csrf": {csrf}, "email": {email}}
	linkPath := srvURL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/players/" + strconv.FormatInt(playerID, 10) + "/link"
	req, err := http.NewRequest(http.MethodPost, linkPath, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	return resp
}

// TestLinkPlayer_resend_supersedes_prior_open_invite: re-linking a player (the admin
// re-sends after a mistake) revokes the first invite and leaves exactly one open
// invite. Two back-to-back sends to the same address are allowed by the recipient
// burst (2); the revoke-then-insert supersession keeps the second send from tripping
// the (project_id, email) open-invite unique index.
func TestLinkPlayer_resend_supersedes_prior_open_invite(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-resend")

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	resp1 := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "resend@example.com")
	resp1.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp1.StatusCode)
	resp2 := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "resend@example.com")
	resp2.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp2.StatusCode)
	require.Len(t, rec.Sent, 2)

	var total, open, revoked int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT
		   count(*),
		   count(*) FILTER (WHERE accepted_at IS NULL AND revoked_at IS NULL),
		   count(*) FILTER (WHERE revoked_at IS NOT NULL)
		 FROM player_invitations WHERE project_player_id = $1`, playerID).Scan(&total, &open, &revoked))
	assert.Equal(t, int64(2), total)
	assert.Equal(t, int64(1), open, "exactly one open invite after resend")
	assert.Equal(t, int64(1), revoked, "prior invite revoked")
}

// TestLinkPlayer_list_shows_pending_badge_and_gates_button: the players list shows
// an "invite pending" badge + a Link button for an unverified row with an open
// invite, and shows no Link button for an already-verified row.
func TestLinkPlayer_list_shows_pending_badge_and_gates_button(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")

	pendingPlayer := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-pending")
	var verifiedPlayer int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO project_players (tenant_id, project_id, external_id, email, email_verified_at)
		 VALUES ($1, $2, 'player-verified', 'already@example.com', now()) RETURNING id`,
		tenantA, projectA).Scan(&verifiedPlayer))

	srv, _ := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	// Put an open invite on the unverified player.
	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, pendingPlayer, "pending@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// Fetch the list page.
	listPath := srv.URL + "/v1/control-panel/tenants/" + strconv.FormatInt(tenantA, 10) +
		"/projects/" + strconv.FormatInt(projectA, 10) + "/players"
	req, err := http.NewRequest(http.MethodGet, listPath, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	listResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	require.Equal(t, http.StatusOK, listResp.StatusCode, string(listBody))
	html := string(listBody)

	assert.Contains(t, html, "invite pending")
	assert.Contains(t, html, "/players/"+strconv.FormatInt(pendingPlayer, 10)+"/link",
		"unverified row should offer a Link button")
	assert.NotContains(t, html, "/players/"+strconv.FormatInt(verifiedPlayer, 10)+"/link",
		"verified row should not offer a Link button")
}

// TestLinkPlayer_accept_conflicts_when_row_linked_to_other_account: if the target
// row is already linked to a different account, acceptance is a 409 and the row is
// left untouched (the BindPlayerLinkedEmail account guard).
func TestLinkPlayer_accept_conflicts_when_row_linked_to_other_account(t *testing.T) {
	c := startCluster(t)
	tenantA, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-a")
	adminID := seedControlPanelUser(t, c, "admin@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantA, "admin")
	playerID := seedExternalIDOnlyPlayer(t, c, tenantA, projectA, "player-guard")

	// Pre-link the target row to some other account.
	var otherAccount string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ('other@example.com', $1, now()) RETURNING id::text`, []byte{0}).Scan(&otherAccount))
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET player_account_id = $1 WHERE id = $2`, otherAccount, playerID)
	require.NoError(t, err)

	srv, rec := newControlPanelAndPlayerServer(t, c)
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.com", "correct-horse-battery-staple")

	resp := postLink(t, srv.URL, cookie, csrf, tenantA, projectA, playerID, "newlink@example.com")
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, rec.Sent, 1)

	status, body := acceptInvite(t, srv.URL, rec.Sent[0].Body)
	assert.Equal(t, http.StatusConflict, status, body)

	// Row unchanged: still linked to the other account, still no email.
	var email, account *string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT email::text, player_account_id::text FROM project_players WHERE id = $1`, playerID).Scan(&email, &account))
	assert.Nil(t, email)
	require.NotNil(t, account)
	assert.Equal(t, otherAccount, *account)
}
