//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

// linkFromBody pulls the first URL path starting with marker out of an email
// body.
func linkFromBody(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	require.GreaterOrEqual(t, i, 0, "email should contain %q: %q", marker, body)
	rest := body[i:]
	end := strings.IndexAny(rest, " \n\r\t")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func TestInviteEmails_name_the_tenant_and_game_and_unsubscribe_suppresses(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-unsub")
	adminID := seedControlPanelUser(t, c, "inviter@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "owner")
	// Named tenant + project so the email copy can be asserted.
	_, err := c.bootstrapPool.Exec(ctx, `UPDATE tenants SET name = 'Acme Games' WHERE id = $1`, tenantID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(ctx, `UPDATE projects SET name = 'Doomerang' WHERE id = $1`, projectID)
	require.NoError(t, err)

	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "inviter@example.com", "correct-horse-battery-staple")

	// T1/T3: the player invite names the game and the inviter, explains
	// ggscale, puts the expiry at the bottom, and carries the unsubscribe
	// link + RFC 8058 header value.
	sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "target@example.com")
	sent := rec.Snapshot()
	require.Len(t, sent, 1)
	playerMail := sent[0]
	assert.Equal(t, "You're invited to play Doomerang", playerMail.Subject)
	assert.Contains(t, playerMail.Body, "inviter@example.com invited you to play Doomerang")
	assert.Contains(t, playerMail.Body, "ggscale is the player-account service")
	assert.Contains(t, playerMail.Body, "This invite expires")
	assert.NotEmpty(t, playerMail.ListUnsubscribe)
	expiryAt := strings.Index(playerMail.Body, "This invite expires")
	linkAt := strings.Index(playerMail.Body, "/v1/players/p/")
	assert.Greater(t, expiryAt, linkAt, "expiry must come after the call to action")

	// T1: the team invite names the tenant.
	form := url.Values{"_csrf": {csrf}, "email": {"teammate@example.com"}, "role": {"tenant_member"}}
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/tenants/"+itoa64(tenantID)+"/team/invite", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	sent = rec.Snapshot()
	require.Len(t, sent, 2)
	teamMail := sent[1]
	assert.Equal(t, "You've been invited to Acme Games on ggscale", teamMail.Subject)
	assert.Contains(t, teamMail.Body, "You were invited to join Acme Games on ggscale")
	assert.Contains(t, teamMail.Body, "read-only access")
	assert.NotEmpty(t, teamMail.ListUnsubscribe)

	// T2: the one-click unsubscribe flow. GET shows a signed-out confirm
	// page; POST suppresses the address.
	unsubLink := linkFromBody(t, playerMail.Body, "/v1/unsubscribe?token=")
	resp, err = http.Get(srv.URL + unsubLink)
	require.NoError(t, err)
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(page))
	assert.Contains(t, string(page), "target@example.com")

	// A tampered token is rejected and suppresses nothing.
	resp, err = http.Post(srv.URL+unsubLink+"x", "application/x-www-form-urlencoded",
		strings.NewReader("List-Unsubscribe=One-Click"))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// RFC 8058 one-click POST (as a mail client would send it).
	resp, err = http.Post(srv.URL+unsubLink, "application/x-www-form-urlencoded",
		strings.NewReader("List-Unsubscribe=One-Click"))
	require.NoError(t, err)
	done, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(done))

	var suppressed bool
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM email_suppressions WHERE email = 'target@example.com')`).Scan(&suppressed))
	require.True(t, suppressed)

	// Future invite email to the suppressed address is dropped — the invite
	// row is still created, only the delivery stops. Clear the first invite
	// so the open-invite unique index admits the resend.
	_, err = c.bootstrapPool.Exec(ctx, `DELETE FROM player_invitations WHERE email = 'target@example.com'`)
	require.NoError(t, err)
	form = url.Values{"_csrf": {csrf}, "email": {"target@example.com"}}
	req, err = http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/tenants/"+itoa64(tenantID)+"/projects/"+itoa64(projectID)+"/players/invite",
		strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Len(t, rec.Snapshot(), 2, "invite email to a suppressed address must be dropped")
	var invites int64
	require.NoError(t, c.bootstrapPool.QueryRow(ctx,
		`SELECT count(*) FROM player_invitations WHERE email = 'target@example.com'`).Scan(&invites))
	assert.Equal(t, int64(1), invites, "the invite itself is still created")
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func TestUnsubscribe_post_is_rate_limited_per_ip(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-rl")
	// Default limiter on purpose: the endpoint must sit behind the signed-out
	// per-IP cap like every other public surface.
	srv, _ := newControlPanelAndPlayerServer(t, c)

	var got429 bool
	for range 15 {
		resp, err := http.Post(srv.URL+"/v1/unsubscribe?token=bogus", "application/x-www-form-urlencoded",
			strings.NewReader("List-Unsubscribe=One-Click"))
		require.NoError(t, err)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429, "unlimited replay against the public unsubscribe write must be throttled")
}

func TestPlayerInvite_survives_header_hostile_project_name(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-evil")
	adminID := seedControlPanelUser(t, c, "evil-inviter@example.com", "correct-horse-battery-staple", false)
	seedControlPanelMembership(t, c, adminID, tenantID, "admin")
	// A project name no mail header may carry: the invite must still be
	// delivered, with the generic fallback wording.
	_, err := c.bootstrapPool.Exec(ctx, `UPDATE projects SET name = E'Evil\nGame' WHERE id = $1`, projectID)
	require.NoError(t, err)

	srv, rec := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})
	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "evil-inviter@example.com", "correct-horse-battery-staple")

	sendPlayerInvite(t, srv, rec, cookie, csrf, tenantID, projectID, "victim2@example.com")
	sent := rec.Snapshot()
	require.Len(t, sent, 1, "a hostile project name must degrade the copy, not break delivery")
	assert.Equal(t, "You're invited to play a game on ggscale", sent[0].Subject)
	assert.NotContains(t, sent[0].Subject, "\n")
	assert.NotContains(t, sent[0].Body, "Evil\nGame")
}
