//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
)

type mcpPanel struct {
	srv      *httptest.Server
	raw      *pgxpool.Pool
	authz    *rbac.Authorizer
	tenantID int64
	project  int64
}

// newMCPPanel mounts the control panel with the MCP feature on and a limit of
// two active tokens for each project.
func newMCPPanel(t *testing.T, enabled bool) mcpPanel {
	t.Helper()
	pool, raw := startTwoFactorDB(t)
	ctx := context.Background()
	p := mcpPanel{raw: raw}
	require.NoError(t, raw.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('mcp') RETURNING id`).Scan(&p.tenantID))
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
			Mount: true, MCPEnabled: enabled, MCPMaxExpiryDays: 90, MCPMaxProjectTokens: 2,
		},
	}))
	p.srv = httptest.NewServer(root)
	t.Cleanup(p.srv.Close)
	return p
}

// member creates a user with a membership role in the tenant and signs in.
func (p mcpPanel) member(t *testing.T, email, role string) (*http.Client, string, int64) {
	t.Helper()
	id := createVerifiedUser(t, p.raw, email)
	require.NoError(t, p.authz.SetControlPanelMembershipRole(id, p.tenantID, role))
	c, csrf := loginAsAdmin(t, p.srv, p.raw, id, email)
	return c, csrf, id
}

func (p mcpPanel) tokensURL() string {
	return p.srv.URL + pathControlPanel + "/tenants/" + strconv.FormatInt(p.tenantID, 10) +
		"/projects/" + strconv.FormatInt(p.project, 10) + "/mcp-tokens"
}

func (p mcpPanel) create(t *testing.T, c *http.Client, csrf string, extra url.Values) (*http.Response, string) {
	t.Helper()
	form := url.Values{"_csrf": {csrf}, "label": {"agent"}, "days": {"30"}}
	for k, v := range extra {
		form[k] = v
	}
	return tfPostForm(t, c, p.tokensURL(), form)
}

func (p mcpPanel) tokenCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM mcp_tokens WHERE project_id = $1`, p.project).Scan(&n))
	return n
}

func TestMCPTokens_owner_creates_token_and_sees_it_once(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, body := p.create(t, c, csrf, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "ggm_")
}

func TestMCPTokens_should_refuse_days_above_maximum(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"days": {"91"}})

	assert.Equal(t, []int{http.StatusUnprocessableEntity, 0}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestMCPTokens_should_refuse_create_at_token_limit(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, nil)
	p.create(t, c, csrf, nil)

	resp, _ := p.create(t, c, csrf, nil)

	assert.Equal(t, []int{http.StatusConflict, 2}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestMCPTokens_should_refuse_scope_the_creator_cannot_use(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "admin@example.com", "admin")
	// Take the publishable key pair away from tenant admins.
	_, err := p.raw.Exec(context.Background(),
		`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'api_key:publishable'`)
	require.NoError(t, err)
	require.NoError(t, p.authz.ReloadPolicy())

	resp, _ := p.create(t, c, csrf, url.Values{"scopes": {"keys:create"}})

	assert.Equal(t, []int{http.StatusForbidden, 0}, []int{resp.StatusCode, p.tokenCount(t)})
}

func TestMCPTokens_should_refuse_unknown_scope(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := p.create(t, c, csrf, url.Values{"scopes": {"tenant:admin"}})

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestMCPTokens_member_gets_403_on_page_and_create(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "member@example.com", "member")

	getResp, _ := tfGet(t, c, p.tokensURL())
	postResp, _ := p.create(t, c, csrf, nil)

	assert.Equal(t, []int{http.StatusForbidden, http.StatusForbidden}, []int{getResp.StatusCode, postResp.StatusCode})
}

func TestMCPTokens_page_is_hidden_when_feature_is_off(t *testing.T) {
	p := newMCPPanel(t, false)
	c, _, _ := p.member(t, "owner@example.com", "owner")

	resp, _ := tfGet(t, c, p.tokensURL())

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMCPTokens_platform_admin_token_has_one_project_and_listed_scopes(t *testing.T) {
	p := newMCPPanel(t, true)
	id := createPlatformAdminUser(t, p.raw, "pa@example.com")
	require.NoError(t, p.authz.ReloadPolicy())
	c, csrf := loginAsAdmin(t, p.srv, p.raw, id, "pa@example.com")

	resp, _ := p.create(t, c, csrf, url.Values{"preset": {"read_write"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var projectID int64
	var scopes []string
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT project_id, scopes FROM mcp_tokens WHERE created_by_user_id = $1`, id).Scan(&projectID, &scopes))
	assert.Equal(t, p.project, projectID)
	assert.ElementsMatch(t, []string{"config:write", "leaderboards:write", "keys:create", "origins:write"}, scopes)
}

func TestMCPTokens_revoke_sets_revoked_at(t *testing.T) {
	p := newMCPPanel(t, true)
	c, csrf, _ := p.member(t, "owner@example.com", "owner")
	p.create(t, c, csrf, nil)
	var id int64
	require.NoError(t, p.raw.QueryRow(context.Background(), `SELECT id FROM mcp_tokens`).Scan(&id))

	resp, _ := tfPostForm(t, c, p.tokensURL()+"/"+strconv.FormatInt(id, 10)+"/revoke", url.Values{"_csrf": {csrf}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	var revoked bool
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT revoked_at IS NOT NULL FROM mcp_tokens WHERE id = $1`, id).Scan(&revoked))
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
