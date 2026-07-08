package controlpanel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSafeReturnPath(t *testing.T) {
	const fallback = "/v1/control-panel/tenants/1/projects"
	cases := []struct {
		raw  string
		want string
	}{
		{"/v1/control-panel/tenants/5/settings", "/v1/control-panel/tenants/5/settings"},
		{"/v1/control-panel", "/v1/control-panel"},
		{"/v1/control-panel/tenants/5/projects/9/settings", "/v1/control-panel/tenants/5/projects/9/settings"},
		{"", fallback},
		{"http://evil.com/x", fallback},
		{"//evil.com", fallback},
		{"/etc/passwd", fallback},
		{"javascript:alert(1)", fallback},
		{"/v1/control-panelX/evil", fallback},
		// Callers append "?flash=", so a raw query or fragment would corrupt
		// the redirect URL.
		{"/v1/control-panel/tenants/5/settings?x=1", fallback},
		{"/v1/control-panel/tenants/5/settings#frag", fallback},
		// Dot segments would let the browser normalize past /v1/control-panel.
		{"/v1/control-panel/../../metrics", fallback},
		{"/v1/control-panel/./evil", fallback},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, safeReturnPath(c.raw, fallback), "raw=%q", c.raw)
	}
}

func TestTenantSettingsPage_renders_editable_forms_with_return_path(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme", Tier: "free",
		IsPlatformAdmin: true, PublicJoining: true,
		APIDefaultRate: 10, APIDefaultBurst: 20,
	}))
	// Public-joining master switch posts to the tenant endpoint...
	assert.Contains(t, html, "/v1/control-panel/tenants/3/public-joining")
	assert.Contains(t, html, "Disable for all projects")
	// API limit form is present for platform admins.
	assert.Contains(t, html, "/v1/control-panel/tenants/3/rate-limits/api")
	assert.Contains(t, html, "Save API limit")
	// Both forms carry a redirect_to back to the settings page.
	assert.Contains(t, html, `name="redirect_to" value="/v1/control-panel/tenants/3/settings"`)
}

func TestTenantSettingsPage_hides_api_form_for_non_platform_admin(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, IsPlatformAdmin: false,
	}))
	assert.NotContains(t, html, "Save API limit")
	assert.Contains(t, html, "Only platform admins")
}

func TestProjectSettingsPage_shows_effective_join_and_forms(t *testing.T) {
	html := renderToString(t, ProjectSettingsPage(ProjectSettingsView{
		TenantID: 3, ProjectID: 8, ProjectName: "arcade",
		TenantPublicJoining: true, ProjectPublicJoining: false,
		DefaultInviterHour: 5, DefaultDomainDay: 50,
	}))
	// Tenant on + project off ⇒ invite only.
	assert.Contains(t, html, "invite only")
	assert.Contains(t, html, "/v1/control-panel/tenants/3/projects/8/public-joining")
	assert.Contains(t, html, "/v1/control-panel/tenants/3/rate-limits/projects/8/invites")
	assert.Contains(t, html, `name="redirect_to" value="/v1/control-panel/tenants/3/projects/8/settings"`)
}

func TestServerSettingsPage_is_read_only_and_redacts_secrets(t *testing.T) {
	html := renderToString(t, ServerSettingsPage(ServerSettingsView{
		Snapshot: ServerSettingsSnapshot{
			Env: "production", HTTPAddr: ":8080",
			ControlPanelEnabled: true,
			SMTPPasswordSet:     true, JWTConfigured: true, DatabaseConfigured: true,
			RelaySecretSet: false,
		},
	}))
	assert.Contains(t, html, "production")
	assert.Contains(t, html, "configured")
	assert.Contains(t, html, "not set")
	// Read-only: no settings forms or editable controls (the only <form> is the
	// layout's logout form in the header chrome).
	assert.Equal(t, 1, strings.Count(html, "<form"), "server settings page adds no forms")
	assert.NotContains(t, html, `name="redirect_to"`)
	assert.NotContains(t, html, "Save")
}
