//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/controlpanel"
)

func TestPlatformUsers_lists_for_platform_admin(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	seedControlPanelUser(t, c, "alice@example.test", "correct-horse-battery-staple", false)
	seedControlPanelUser(t, c, "bob@example.test", "correct-horse-battery-staple", false)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/admin/users", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "admin@example.test")
	assert.Contains(t, string(body), "alice@example.test")
	assert.Contains(t, string(body), "bob@example.test")
}

func TestPlatformUsers_forbidden_for_non_platform_admin(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "regular@example.test", "correct-horse-battery-staple", false)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "regular@example.test", "correct-horse-battery-staple")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/admin/users", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPlatformUsers_search_filters_by_email(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	seedControlPanelUser(t, c, "alice@example.test", "correct-horse-battery-staple", false)
	seedControlPanelUser(t, c, "bob@example.test", "correct-horse-battery-staple", false)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	cookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel/admin/users?q=alice", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "alice@example.test")
	assert.NotContains(t, string(body), "bob@example.test")
}

func TestPlatformUsers_disable_revokes_sessions_invites_and_writes_audit(t *testing.T) {
	c := startCluster(t)
	adminID := seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	require.Greater(t, adminID, int64(0))

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())

	// Target has an active control_panel_session.
	targetCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "victim@example.test", "correct-horse-battery-staple")
	require.NotNil(t, targetCookie)

	// Target sent an open invite that should be auto-revoked.
	openInviteID := seedControlPanelInvitation(t, c, targetID, "guest@example.test", "tenant_member", false /*revoked*/, false /*accepted*/)
	// Target accepted one previously; that row stays untouched.
	acceptedInviteID := seedControlPanelInvitation(t, c, targetID, "alreadyhere@example.test", "tenant_member", false, true)

	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")
	form := url.Values{"_csrf": {adminCSRF}}
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/admin/users/"+strconv.FormatInt(targetID, 10)+"/disable",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	// disabled_at is set.
	var disabledAt *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at FROM control_panel_users WHERE id = $1`, targetID).Scan(&disabledAt))
	assert.NotNil(t, disabledAt)

	// Active sessions revoked.
	var liveSessions int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM control_panel_sessions WHERE control_panel_user_id = $1 AND revoked_at IS NULL`,
		targetID).Scan(&liveSessions))
	assert.Equal(t, int64(0), liveSessions, "active sessions should be revoked on disable")

	// Open outgoing invite revoked.
	var openRevoked *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM control_panel_invitations WHERE id = $1`, openInviteID).Scan(&openRevoked))
	assert.NotNil(t, openRevoked, "open outgoing invite should be revoked")

	// Accepted invite untouched.
	var acceptedRevoked *time.Time
	var acceptedAt *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT revoked_at, accepted_at FROM control_panel_invitations WHERE id = $1`, acceptedInviteID).Scan(&acceptedRevoked, &acceptedAt))
	assert.Nil(t, acceptedRevoked, "accepted invites should not be revoked on inviter disable")
	assert.NotNil(t, acceptedAt)

	// Audit row exists.
	var auditAction string
	var auditPayload string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT action, payload::text FROM platform_audit_log WHERE action = 'control_panel.user.disabled' ORDER BY id DESC LIMIT 1`).
		Scan(&auditAction, &auditPayload))
	assert.Equal(t, "control_panel.user.disabled", auditAction)
	assert.Contains(t, auditPayload, "victim@example.test")
}

func TestPlatformUsers_disabled_user_cannot_login_returns_401(t *testing.T) {
	c := startCluster(t)
	adminID := seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	require.Greater(t, adminID, int64(0))
	disableControlPanelUser(t, c, targetID)

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	form := url.Values{"email": {"victim@example.test"}, "password": {"correct-horse-battery-staple"}}
	resp, err := noRedirectClient().Post(srv.URL+"/v1/control-panel/login", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	require.NoError(t, err)
	defer resp.Body.Close()
	// 401 (treated like unknown email), not 423 (account locked).
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPlatformUsers_disabled_users_cookie_redirects_to_login(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())

	// Get a real session cookie for the victim BEFORE we disable them.
	victimCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "victim@example.test", "correct-horse-battery-staple")
	disableControlPanelUser(t, c, targetID)

	// Reuse the stale cookie — should redirect to /login.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel", nil)
	req.AddCookie(victimCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/control-panel/login", resp.Header.Get("Location"))
}

func TestPlatformUsers_enable_clears_disabled_at_does_not_revive_invites(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	disableControlPanelUser(t, c, targetID)
	// Pre-existing revoked invite that we created via the disable flow.
	revokedInviteID := seedControlPanelInvitation(t, c, targetID, "guest@example.test", "tenant_member", true /*revoked*/, false)

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")

	form := url.Values{"_csrf": {adminCSRF}}
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/admin/users/"+strconv.FormatInt(targetID, 10)+"/enable",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var disabledAt *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at FROM control_panel_users WHERE id = $1`, targetID).Scan(&disabledAt))
	assert.Nil(t, disabledAt)

	var revokedAt *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM control_panel_invitations WHERE id = $1`, revokedInviteID).Scan(&revokedAt))
	assert.NotNil(t, revokedAt, "enable must NOT un-revoke previously-revoked invitations")

	var auditAction string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT action FROM platform_audit_log WHERE action = 'control_panel.user.enabled' ORDER BY id DESC LIMIT 1`).
		Scan(&auditAction))
	assert.Equal(t, "control_panel.user.enabled", auditAction)
}

func TestPlatformUsers_self_disable_blocked(t *testing.T) {
	c := startCluster(t)
	adminID := seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	adminCookie, adminCSRF := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")

	form := url.Values{"_csrf": {adminCSRF}}
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/admin/users/"+strconv.FormatInt(adminID, 10)+"/disable",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	// 303 with flash; not 500.
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var disabledAt *time.Time
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT disabled_at FROM control_panel_users WHERE id = $1`, adminID).Scan(&disabledAt))
	assert.Nil(t, disabledAt, "self-disable must not mutate the row")

	var count int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM platform_audit_log WHERE action = 'control_panel.user.disabled'`).Scan(&count))
	assert.Equal(t, int64(0), count)
}

func TestPlatformUsers_disable_csrf_required(t *testing.T) {
	c := startCluster(t)
	seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	adminCookie, _ := controlPanelLoginCookieAndCSRF(t, srv.URL, "admin@example.test", "correct-horse-battery-staple")

	// Note: no CSRF token in form.
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/control-panel/admin/users/"+strconv.FormatInt(targetID, 10)+"/disable",
		strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(adminCookie)
	resp, err := noRedirectClient().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPlatformUsers_invite_accept_for_disabled_account_friendly_error(t *testing.T) {
	c := startCluster(t)
	adminID := seedControlPanelUser(t, c, "admin@example.test", "correct-horse-battery-staple", true)
	targetID := seedControlPanelUser(t, c, "victim@example.test", "correct-horse-battery-staple", false)
	disableControlPanelUser(t, c, targetID)

	// Hand-seed a (still-open) invitation against the disabled user's email.
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "some-key")
	code := "raw-test-invite-code-very-long-for-security"
	codeHash := sha256.Sum256([]byte(":" + code)) // matches verifycode.Hash(nil, code)
	expiresAt := time.Now().Add(72 * time.Hour)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO control_panel_invitations (email, tenant_id, role, code_hash, expires_at, invited_by_user_id)
		 VALUES ($1, $2, 'tenant_admin', $3, $4, $5)`,
		"victim@example.test", tenantID, codeHash[:], expiresAt, adminID)
	require.NoError(t, err)

	srv := newControlPanelIntegrationServer(t, c, controlpanel.DisabledBootstrap())
	resp, err := http.Get(srv.URL + "/v1/control-panel/invite/accept?code=" + url.QueryEscape(code))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "disabled")
}

// seedControlPanelInvitation inserts a control_panel_invitations row with
// optional revoked/accepted flags pre-set. Returns the new id.
func seedControlPanelInvitation(t *testing.T, c *cluster, inviterID int64, email, role string, revoked, accepted bool) int64 {
	t.Helper()
	codeHash := sha256.Sum256([]byte(email + role)) // arbitrary unique hash per row
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO control_panel_invitations
		   (email, tenant_id, role, code_hash, expires_at, invited_by_user_id, revoked_at, accepted_at)
		 VALUES ($1, NULL, $2, $3, now() + interval '7 days', $4,
		         CASE WHEN $5 THEN now() ELSE NULL END,
		         CASE WHEN $6 THEN now() ELSE NULL END)
		 RETURNING id`,
		email, role, codeHash[:], inviterID, revoked, accepted).Scan(&id))
	return id
}

// disableControlPanelUser flips disabled_at to now() for the given user.
func disableControlPanelUser(t *testing.T, c *cluster, userID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE control_panel_users SET disabled_at = now() WHERE id = $1`, userID)
	require.NoError(t, err)
}
