//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
)

type tokenPanel struct {
	srv      *httptest.Server
	raw      *pgxpool.Pool
	authz    *rbac.Authorizer
	tenantID int64
	project  int64
}

// newTokenPanel mounts the control panel with the management API feature set
// to enabled and a limit of two active tokens for each project.
func newTokenPanel(t *testing.T, enabled bool) tokenPanel {
	t.Helper()
	return newTokenPanelWithFleet(t, enabled, false)
}

// newTokenPanelWithFleet is newTokenPanel with the fleet server switch set.
func newTokenPanelWithFleet(t *testing.T, enabled, fleetEnabled bool) tokenPanel {
	t.Helper()
	pool, raw := startTwoFactorDB(t)
	ctx := context.Background()
	p := tokenPanel{raw: raw}
	require.NoError(t, raw.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('management') RETURNING id`).Scan(&p.tenantID))
	p.project = createLeaderboardProject(t, raw, p.tenantID, "game")

	var err error
	p.authz, err = rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(p.authz.Close)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)
	credCipher, err := secretseal.Load(ctx, pool, "")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool: pool, Mailer: noopMailer, RBAC: p.authz, CredentialCipher: credCipher,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
		Config: controlpanel.Config{
			Mount: true, ManagementAPIEnabled: enabled, FleetEnabled: fleetEnabled, ManagementMaxExpiryDays: 90, ManagementMaxProjectTokens: 2,
		},
	}))
	p.srv = httptest.NewServer(root)
	t.Cleanup(p.srv.Close)
	return p
}

// member creates a user with a membership role in the tenant and signs in.
func (p tokenPanel) member(t *testing.T, email, role string) (*http.Client, string, int64) {
	t.Helper()
	id := createVerifiedUser(t, p.raw, email)
	require.NoError(t, p.authz.SetControlPanelMembershipRole(id, p.tenantID, role))
	c, csrf := loginAsAdmin(t, p.srv, p.raw, id, email)
	return c, csrf, id
}

func (p tokenPanel) tokensURL() string {
	return p.srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(p.tenantID, 10) +
		"/projects/" + strconv.FormatInt(p.project, 10) + "/management-tokens"
}

func (p tokenPanel) create(t *testing.T, c *http.Client, csrf string, extra url.Values) (*http.Response, string) {
	t.Helper()
	form := url.Values{"_csrf": {csrf}, "label": {"agent"}, "days": {"30"}}
	for k, v := range extra {
		form[k] = v
	}
	return tfPostForm(t, c, p.tokensURL(), form)
}

func (p tokenPanel) tokenCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM management_tokens WHERE project_id = $1`, p.project).Scan(&n))
	return n
}

func TestManagementTokens_owner_creates_token_and_sees_it_once(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, body := p.create(t, c, csrf, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "ggm_")
}

func TestManagementTokens_should_refuse_days_above_maximum(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"days": {"91"}})

	assert.Equal(t, []int{http.StatusUnprocessableEntity, 0}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestManagementTokens_should_refuse_create_at_token_limit(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, nil)
	p.create(t, c, csrf, nil)

	resp, _ := p.create(t, c, csrf, nil)

	assert.Equal(t, []int{http.StatusConflict, 2}, []int{resp.StatusCode, p.tokenCount(t)})
}

// dropAdminPair takes the Casbin pair (obj, any action) away from tenant
// admins.
func (p tokenPanel) dropAdminPair(t *testing.T, obj string) {
	t.Helper()
	_, err := p.raw.Exec(context.Background(),
		`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = $1`, obj)
	require.NoError(t, err)
	require.NoError(t, p.authz.ReloadPolicy())
}

func TestManagementTokens_should_refuse_a_disabled_option_on_post(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "admin@example.com", "admin")
	p.dropAdminPair(t, "api_key:publishable")

	resp, _ := p.create(t, c, csrf, url.Values{"access_keys": {"write"}})

	assert.Equal(t, []int{http.StatusForbidden, 0}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestManagementTokens_page_should_show_the_role_reason_for_a_disabled_option(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "admin@example.com", "admin")
	p.dropAdminPair(t, "api_key:publishable")

	_, body := tfGet(t, c, p.tokensURL())

	assert.Contains(t, body, "Your role does not allow this.")
}

func (p tokenPanel) onlyTokenScopes(t *testing.T) []string {
	t.Helper()
	var scopes []string
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT scopes FROM management_tokens WHERE project_id = $1`, p.project).Scan(&scopes))
	return scopes
}

var checkedRadio = regexp.MustCompile(`name="(access_[a-z]+)" value="([a-z]+)"[^>]*checked`)

// checkedAccess returns the access_<resource> values that the form in body
// sends as it is.
func checkedAccess(body string) url.Values {
	out := url.Values{}
	for _, m := range checkedRadio.FindAllStringSubmatch(body, -1) {
		out.Set(m[1], m[2])
	}
	return out
}

func TestManagementTokens_default_form_should_make_a_read_only_token(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	_, page := tfGet(t, c, p.tokensURL())

	p.create(t, c, csrf, checkedAccess(page))

	// The fleets row is turned off on this server, so its read is not given.
	want := slices.DeleteFunc(managementtoken.ReadScopes(), func(s string) bool { return s == managementtoken.ScopeFleetRead })
	assert.ElementsMatch(t, want, p.onlyTokenScopes(t))
}

func TestManagementTokens_fleets_row_should_say_why_it_is_disabled(t *testing.T) {
	cases := []struct {
		name  string
		fleet bool
		want  string
	}{
		{name: "should_say_turned_off_when_the_switch_is_off", fleet: false, want: "Turned off on this server."},
		{name: "should_say_not_enabled_when_the_grant_is_missing", fleet: true, want: "Not enabled for this Account Tenant."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTokenPanelWithFleet(t, true, tc.fleet)
			c, _, _ := p.member(t, "owner@example.com", "owner")

			_, body := tfGet(t, c, p.tokensURL())

			assert.Contains(t, body, tc.want)
		})
	}
}

func TestManagementTokens_fleets_row_should_give_fleet_read_with_the_switch_and_the_grant(t *testing.T) {
	p := newTokenPanelWithFleet(t, true, true)
	_, err := p.raw.Exec(context.Background(), `INSERT INTO feature_grants (tenant_id, feature, enabled) VALUES ($1, 'dedicated_servers', true)`, p.tenantID)
	require.NoError(t, err)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	_, page := tfGet(t, c, p.tokensURL())

	p.create(t, c, csrf, checkedAccess(page))

	assert.ElementsMatch(t, managementtoken.ReadScopes(), p.onlyTokenScopes(t))
}

func TestManagementTokens_write_row_should_give_its_read_and_write_scopes(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	p.create(t, c, csrf, url.Values{"access_config": {"write"}, "access_keys": {"read"}})

	assert.ElementsMatch(t, []string{"config:read", "config:write", "origins:write", "keys:read"}, p.onlyTokenScopes(t))
}

func TestManagementTokens_quick_fill_read_write_should_select_write_rows(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	_, body := tfGet(t, c, p.tokensURL()+"?fill=read_write")

	assert.Equal(t, url.Values{"access_config": {"write"}, "access_leaderboards": {"write"}, "access_keys": {"write"}, "access_matchmaking": {"read"}, "access_players": {"write"}, "access_limits": {"write"}, "access_fleets": {"none"}},
		checkedAccess(body))
}

var likeLink = regexp.MustCompile(`href="([^"]*)"[^>]*>New token like this<`)

func TestManagementTokens_new_token_like_this_should_fill_the_same_access(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	access := url.Values{"access_config": {"write"}, "access_leaderboards": {"none"}, "access_keys": {"read"}, "access_matchmaking": {"none"}, "access_players": {"none"}, "access_limits": {"none"}, "access_fleets": {"none"}}
	p.create(t, c, csrf, access)
	_, list := tfGet(t, c, p.tokensURL())
	m := likeLink.FindStringSubmatch(list)
	require.NotNil(t, m, list)

	_, body := tfGet(t, c, p.srv.URL+html.UnescapeString(m[1]))

	assert.Equal(t, access, checkedAccess(body))
}

func TestManagementTokens_list_should_show_access_in_words(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, url.Values{"access_config": {"write"}, "access_leaderboards": {"read"}})

	_, body := tfGet(t, c, p.tokensURL())

	assert.Contains(t, body, "Remote config and origins: read and write · Leaderboards: read")
}

func TestManagementTokens_list_should_count_scopes_the_creator_lost(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "admin@example.com", "admin")
	p.create(t, c, csrf, url.Values{"access_config": {"write"}, "access_keys": {"write"}})
	p.dropAdminPair(t, "project:*:config")

	_, body := tfGet(t, c, p.tokensURL())

	assert.Contains(t, body, "2 permissions inactive: creator lost access")
}

func TestManagementTokens_should_refuse_an_unknown_access_value(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"access_config": {"admin"}})

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestManagementTokens_should_refuse_a_normal_scope_as_risky(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"risky": {"config:write"}})

	assert.Equal(t, []int{http.StatusForbidden, 0}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestManagementTokens_created_page_should_show_the_access_in_words(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	_, body := p.create(t, c, csrf, url.Values{"access_leaderboards": {"write"}})

	assert.Contains(t, body, "Access: Leaderboards: read and write")
}

func TestManagementTokens_member_gets_403_on_page_and_create(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "member@example.com", "member")

	getResp, _ := tfGet(t, c, p.tokensURL())
	postResp, _ := p.create(t, c, csrf, nil)

	assert.Equal(t, []int{http.StatusForbidden, http.StatusForbidden}, []int{getResp.StatusCode, postResp.StatusCode})
}

func TestManagementTokens_page_is_hidden_when_feature_is_off(t *testing.T) {
	p := newTokenPanel(t, false)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := tfGet(t, c, p.tokensURL())

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestManagementTokens_page_has_management_tokens_title(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	_, body := tfGet(t, c, p.tokensURL())

	assert.Contains(t, body, "<h1>Management tokens</h1>")
}

func TestManagementTokens_old_mcp_tokens_url_returns_404(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")
	oldURL := strings.TrimSuffix(p.tokensURL(), "/management-tokens") + "/mcp-tokens"

	resp, _ := tfGet(t, c, oldURL)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestManagementTokens_create_writes_management_token_audit_action(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, ownerID := p.member(t, "owner@example.com", "owner")

	p.create(t, c, csrf, nil)

	var n int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE action = 'management_token.create' AND actor_user_id = $1`, ownerID).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestManagementTokens_revoke_writes_management_token_audit_action(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, ownerID := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, nil)
	id := p.onlyTokenID(t)

	tfPostForm(t, c, p.tokensURL()+"/"+strconv.FormatInt(id, 10)+"/revoke", url.Values{"_csrf": {csrf}})

	var n int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE action = 'management_token.revoke' AND actor_user_id = $1`, ownerID).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestManagementTokens_platform_admin_token_has_one_project_and_listed_scopes(t *testing.T) {
	p := newTokenPanel(t, true)
	id := createPlatformAdminUser(t, p.raw, "pa@example.com")
	require.NoError(t, p.authz.ReloadPolicy())
	c, csrf := loginAsAdmin(t, p.srv, p.raw, id, "pa@example.com")

	resp, _ := p.create(t, c, csrf, url.Values{
		"access_config": {"write"}, "access_leaderboards": {"write"}, "access_keys": {"write"}, "access_matchmaking": {"read"}, "access_players": {"read"}, "access_limits": {"read"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var projectID int64
	var scopes []string
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT project_id, scopes FROM management_tokens WHERE created_by_user_id = $1`, id).Scan(&projectID, &scopes))
	assert.Equal(t, p.project, projectID)
	reads := slices.DeleteFunc(managementtoken.ReadScopes(), func(s string) bool { return s == managementtoken.ScopeFleetRead })
	assert.ElementsMatch(t, append([]string{"config:write", "leaderboards:write", "keys:create", "keys:write", "origins:write"}, reads...), scopes)
}

func TestManagementTokens_revoke_sets_revoked_at(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, nil)
	var id int64
	require.NoError(t, p.raw.QueryRow(context.Background(), `SELECT id FROM management_tokens`).Scan(&id))

	resp, _ := tfPostForm(t, c, p.tokensURL()+"/"+strconv.FormatInt(id, 10)+"/revoke", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var revoked bool
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM management_tokens WHERE id = $1`, id).Scan(&revoked))
	assert.True(t, revoked)
}

func TestMatchmakerQueue_admin_can_open_queue_but_not_fleets(t *testing.T) {
	pool, raw := startTwoFactorDB(t)
	ctx := context.Background()
	var tenantID int64
	require.NoError(t, raw.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('mm') RETURNING id`).Scan(&tenantID))
	project := createLeaderboardProject(t, raw, tenantID, "game")
	authz, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authz.Close)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)
	credCipher, err := secretseal.Load(ctx, pool, "")
	require.NoError(t, err)
	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool: pool, Mailer: noopMailer, RBAC: authz, CredentialCipher: credCipher,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
		Config:           controlpanel.Config{Mount: true, FleetEnabled: true},
	}))
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	id := createVerifiedUser(t, raw, "admin@example.com")
	require.NoError(t, authz.SetControlPanelMembershipRole(id, tenantID, "admin"))
	c, _ := loginAsAdmin(t, srv, raw, id, "admin@example.com")
	base := srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(project, 10)

	codes := []int{}
	for _, page := range []string{"/matchmaker", "/fleets", "/allocations"} {
		resp, _ := tfGet(t, c, base+page)
		codes = append(codes, resp.StatusCode)
	}

	assert.Equal(t, []int{http.StatusOK, http.StatusForbidden, http.StatusForbidden}, codes)
}

func (p tokenPanel) onlyTokenID(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, p.raw.QueryRow(context.Background(), `SELECT id FROM management_tokens`).Scan(&id))
	return id
}

func (p tokenPanel) revoked(t *testing.T, id int64) bool {
	t.Helper()
	var revoked bool
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM management_tokens WHERE id = $1`, id).Scan(&revoked))
	return revoked
}

func TestManagementTokens_non_creator_without_secret_key_pair_cannot_revoke(t *testing.T) {
	p := newTokenPanel(t, true)
	owner, ownerCSRF, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, owner, ownerCSRF, nil)
	id := p.onlyTokenID(t)
	admin, adminCSRF, _ := p.member(t, "admin@example.com", "admin")
	p.dropAdminPair(t, "api_key:secret")

	resp, _ := tfPostForm(t, admin, p.tokensURL()+"/"+strconv.FormatInt(id, 10)+"/revoke", url.Values{"_csrf": {adminCSRF}})

	assert.Equal(t, []any{http.StatusForbidden, false}, []any{resp.StatusCode, p.revoked(t, id)})
}

func TestManagementTokens_creator_can_revoke_own_token_without_secret_key_pair(t *testing.T) {
	p := newTokenPanel(t, true)
	admin, csrf, _ := p.member(t, "admin@example.com", "admin")
	p.create(t, admin, csrf, nil)
	id := p.onlyTokenID(t)
	p.dropAdminPair(t, "api_key:secret")

	resp, _ := tfPostForm(t, admin, p.tokensURL()+"/"+strconv.FormatInt(id, 10)+"/revoke", url.Values{"_csrf": {csrf}})

	assert.Equal(t, []any{http.StatusSeeOther, true}, []any{resp.StatusCode, p.revoked(t, id)})
}

func TestManagementTokens_list_hides_tokens_expired_over_30_days(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, ownerID := p.member(t, "owner@example.com", "owner")
	_, err := p.raw.Exec(context.Background(), `
		INSERT INTO management_tokens (tenant_id, project_id, created_by_user_id, label, token_hash, token_hint, expires_at) VALUES
		  ($1, $2, $3, 'recently-expired', '\x01'::bytea, 'aaaa', now() - interval '1 day'),
		  ($1, $2, $3, 'long-expired',     '\x02'::bytea, 'bbbb', now() - interval '31 days')`,
		p.tenantID, p.project, ownerID)
	require.NoError(t, err)

	_, body := tfGet(t, c, p.tokensURL())

	assert.Equal(t, []bool{true, false},
		[]bool{strings.Contains(body, "recently-expired"), strings.Contains(body, "long-expired")})
}

func TestAPIKeys_dashboard_label_and_features_should_write_audit_rows_as_the_user(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, ownerID := p.member(t, "owner@example.com", "owner")
	var keyID int64
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, label, scopes)
		 VALUES ($1, $2, sha256('k'::bytea), 'publishable', 'old', '{matchmaker,fleet}') RETURNING id`, p.tenantID, p.project).Scan(&keyID))
	base := p.srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(p.tenantID, 10) + "/api-keys/" + strconv.FormatInt(keyID, 10)

	tfPostForm(t, c, base+"/label", url.Values{"_csrf": {csrf}, "label": {"renamed"}})
	tfPostForm(t, c, base+"/features", url.Values{"_csrf": {csrf}, "scopes": {"matchmaker"}})

	var label string
	var scopes []string
	var audited int
	require.NoError(t, p.raw.QueryRow(context.Background(), `SELECT label, scopes FROM api_keys WHERE id = $1`, keyID).Scan(&label, &scopes))
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE actor_user_id = $1 AND target = $2
		 AND action IN ('control_panel.api_key.relabel', 'control_panel.api_key.scope_revoke')`, ownerID, strconv.FormatInt(keyID, 10)).Scan(&audited))
	assert.Equal(t, []any{"renamed", []string{"matchmaker"}, 2}, []any{label, scopes, audited})
}

func TestManagementTokens_should_refuse_a_risky_scope_the_creator_cannot_give(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "admin@example.com", "admin")
	p.dropAdminPair(t, "project:*:players")

	resp, body := p.create(t, c, csrf, url.Values{"risky": {managementtoken.ScopePlayersPII}})

	assert.Equal(t, []any{http.StatusForbidden, true, 0},
		[]any{resp.StatusCode, strings.Contains(body, `"players:pii"`), p.tokenCount(t)})
}

func TestManagementTokens_should_refuse_pii_without_players_read(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, body := p.create(t, c, csrf, url.Values{"label": {"keep-me"}, "access_players": {"none"}, "risky": {managementtoken.ScopePlayersPII}})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, true, false, 0}, []any{resp.StatusCode,
		strings.Contains(body, "Needs read access to Players. Give that access or uncheck the box."),
		strings.Contains(body, `value="keep-me"`), strings.Contains(body, "A link checked risky access"), p.tokenCount(t)})
}

func TestManagementTokens_should_give_pii_with_players_read(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"access_players": {"read"}, "risky": {managementtoken.ScopePlayersPII}})

	assert.Equal(t, []any{http.StatusOK, []string{managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII}},
		[]any{resp.StatusCode, p.onlyTokenScopes(t)})
}

func TestManagementTokens_page_should_show_the_pii_box_with_its_dependency(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	_, body := tfGet(t, c, p.tokensURL())

	assert.Contains(t, body, "See player email and display name. They go into agent logs.")
	assert.Contains(t, body, "Needs read access to Players")
}

func TestManagementTokens_page_should_warn_when_a_link_checks_a_risky_box(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	_, withLink := tfGet(t, c, p.tokensURL()+"?access_players=read&risky="+managementtoken.ScopePlayersPII)
	_, plain := tfGet(t, c, p.tokensURL())

	assert.Equal(t, []bool{true, false}, []bool{strings.Contains(withLink, "A link checked risky access"), strings.Contains(plain, "A link checked risky access")})
}

func TestProjectLimits_dashboard_writes_should_save_and_audit_the_user(t *testing.T) {
	// Arrange
	p := newTokenPanel(t, true)
	c, csrf, ownerID := p.member(t, "owner@example.com", "owner")
	base := p.srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(p.tenantID, 10) + "/rate-limits/projects/" + strconv.FormatInt(p.project, 10)

	// Act
	tfPostForm(t, c, base+"/storage", url.Values{"_csrf": {csrf}, "max_value_mb": {"0.5"}})
	tfPostForm(t, c, base+"/invites", url.Values{"_csrf": {csrf}, "inviter_per_hour": {"5"}, "domain_per_day": {"7"}})

	// Assert
	var bytes int64
	var overrides int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT max_value_bytes FROM storage_limits WHERE tenant_id = $1 AND project_id = $2`, p.tenantID, p.project).Scan(&bytes))
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM rate_limit_overrides WHERE tenant_id = $1 AND project_id = $2`, p.tenantID, p.project).Scan(&overrides))
	var audited int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE actor_user_id = $1 AND target = $2
		 AND action IN ('control_panel.storage_limit.set', 'control_panel.rate_limit.set')`, ownerID, strconv.FormatInt(p.project, 10)).Scan(&audited))
	assert.Equal(t, []any{int64(524288), 2, 2}, []any{bytes, overrides, audited})
}

func TestManagementTokens_page_should_show_the_key_risky_boxes_in_the_risky_group(t *testing.T) {
	p := newTokenPanel(t, true)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	_, body := tfGet(t, c, p.tokensURL())

	group := strings.Index(body, "Risky access")
	revoke := strings.Index(body, "Revoke API keys. Cannot be undone; a game that uses the key stops.")
	secret := strings.Index(body, "Create secret API keys. The key value is returned to the token user.")
	assert.Equal(t, []bool{true, true, true}, []bool{group >= 0 && revoke > group, secret > group, strings.Contains(body, "Needs read access to API keys")})
}

func TestManagementTokens_should_refuse_keys_revoke_without_keys_read(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, body := p.create(t, c, csrf, url.Values{"access_keys": {"none"}, "risky": {managementtoken.ScopeKeysRevoke}})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, 0}, []any{resp.StatusCode,
		strings.Contains(body, "Needs read access to API keys. Give that access or uncheck the box."), p.tokenCount(t)})
}

func TestManagementTokens_should_give_the_key_risky_scopes_only_when_checked(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"access_keys": {"read"},
		"risky": {managementtoken.ScopeKeysRevoke, managementtoken.ScopeKeysSecret}})

	assert.Equal(t, []any{http.StatusOK, []string{managementtoken.ScopeKeysRead, managementtoken.ScopeKeysRevoke, managementtoken.ScopeKeysSecret}},
		[]any{resp.StatusCode, p.onlyTokenScopes(t)})
}

func TestManagementTokens_should_refuse_keys_secret_when_the_creator_lacks_the_secret_pair(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "admin@example.com", "admin")
	p.dropAdminPair(t, "api_key:secret")

	resp, body := p.create(t, c, csrf, url.Values{"risky": {managementtoken.ScopeKeysSecret}})

	assert.Equal(t, []any{http.StatusForbidden, true, 0},
		[]any{resp.StatusCode, strings.Contains(body, `"keys:secret"`), p.tokenCount(t)})
}

func TestManagementTokens_should_give_steam_write_only_when_checked(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"risky": {managementtoken.ScopeSteamWrite}})

	assert.Equal(t, []any{http.StatusOK, []string{managementtoken.ScopeSteamWrite}}, []any{resp.StatusCode, p.onlyTokenScopes(t)})
}

func TestManagementTokens_should_refuse_invite_without_players_read(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, body := p.create(t, c, csrf, url.Values{"access_players": {"none"}, "risky": {managementtoken.ScopePlayersInvite}})

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, 0}, []any{resp.StatusCode,
		strings.Contains(body, "Needs read access to Players. Give that access or uncheck the box."), p.tokenCount(t)})
}

func TestManagementTokens_should_give_invite_with_players_read(t *testing.T) {
	p := newTokenPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"access_players": {"read"}, "risky": {managementtoken.ScopePlayersInvite}})

	assert.Equal(t, []any{http.StatusOK, []string{managementtoken.ScopePlayersRead, managementtoken.ScopePlayersInvite}},
		[]any{resp.StatusCode, p.onlyTokenScopes(t)})
}
