package controlpanel

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSetTenantTier_rejects_out_of_range_class_before_database_access(t *testing.T) {
	h := &Handler{}

	for _, target := range []int16{-1, 4} {
		changed, err := h.setTenantTier(context.Background(), 1, 2, target)

		assert.False(t, changed, "target=%d", target)
		assert.ErrorIs(t, err, errInvalidTenantTier, "target=%d", target)
	}
}

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

func TestTenantSettingsPage_renders_tier_form_for_platform_admin(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme", Tier: "tier_0",
		IsPlatformAdmin: true,
	}))
	// Players are linked by project admins, never self-join: no public-joining control.
	assert.NotContains(t, html, "public-joining")
	assert.NotContains(t, html, "Public joining")
	// The HTTP API limit card now lives only on the rate-limits page.
	assert.NotContains(t, html, "Save API limit")
	assert.NotContains(t, html, "/v1/control-panel/tenants/3/rate-limits/api")
	// Platform admins can directly move a tenant to any class, including down.
	assert.Contains(t, html, `/v1/control-panel/tenants/3/settings/tier`)
	assert.Contains(t, html, `name="tier"`)
	assert.Contains(t, html, `<option value="0" selected>tier_0</option>`)
	assert.Contains(t, html, `<option value="3">tier_3</option>`)
	// The page links out to the rate-limits page where API/invite limits live.
	assert.Contains(t, html, `/v1/control-panel/tenants/3/rate-limits"`)
}

func TestTenantSettingsPage_hides_tier_form_for_non_platform_admin(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, IsPlatformAdmin: false,
	}))
	assert.NotContains(t, html, `name="tier"`)
	assert.NotContains(t, html, "/settings/tier")
	// The API limit card is not on this page for anyone now.
	assert.NotContains(t, html, "Save API limit")
}

func TestProjectSettingsPage_shows_invite_quota_forms(t *testing.T) {
	html := renderToString(t, ProjectSettingsPage(ProjectSettingsView{
		TenantID: 3, ProjectID: 8, ProjectName: "arcade",
		DefaultInviterHour: 5, DefaultDomainDay: 50,
	}))
	// Players are linked by project admins, never self-join: no public-joining control.
	assert.NotContains(t, html, "public-joining")
	assert.NotContains(t, html, "Public joining")
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

func TestServerSettingsPage_shows_database_stored_badge_for_zero_config_jwt_key(t *testing.T) {
	html := renderToString(t, ServerSettingsPage(ServerSettingsView{
		Snapshot: ServerSettingsSnapshot{JWTConfigured: false},
	}))

	// The auto-generated key persists in server_secrets: informational, not
	// the alarm styling used for genuinely missing secrets.
	assert.Contains(t, html, "database-stored")
}
