package controlpanel

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProjectSettingsPage_shows_steam_auth_card(t *testing.T) {
	html := renderToString(t, ProjectSettingsPage(ProjectSettingsView{
		TenantID: 3, ProjectID: 8, ProjectName: "arcade",
		SteamAppID: "480",
	}))
	assert.Contains(t, html, "/v1/control-panel/tenants/3/projects/8/steam-auth")
	assert.Contains(t, html, `name="steam_app_id"`)
	assert.Contains(t, html, `value="480"`)
	assert.Contains(t, html, `name="steam_web_api_key"`)
	assert.Contains(t, html, `type="password"`)
	assert.Contains(t, html, `name="steam_clear"`)
}

func TestProjectSettingsPage_never_renders_steam_key_value(t *testing.T) {
	html := renderToString(t, ProjectSettingsPage(ProjectSettingsView{
		TenantID: 3, ProjectID: 8, ProjectName: "arcade",
		SteamAppID: "480", SteamKeyConfigured: true,
	}))
	// The view model has no key field at all; the password input must stay
	// empty and only signal that a key exists.
	assert.Contains(t, html, "configured")
	assert.NotContains(t, html, `name="steam_web_api_key" value=`)
}
