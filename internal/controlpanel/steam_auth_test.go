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

func TestValidateSteamAppID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"spacewar", "480", false},
		{"large_id", "2358720", false},
		{"empty", "", true},
		{"letters", "48o", true},
		{"negative", "-480", true},
		{"whitespace", " 480", true},
		{"too_long", "12345678901", true},
		{"hex_prefix", "0x1E0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSteamAppID(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateSteamWebAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid_upper", "0123456789ABCDEF0123456789ABCDEF", false},
		{"valid_lower", "0123456789abcdef0123456789abcdef", false},
		{"empty", "", true},
		{"short", "0123456789ABCDEF", true},
		{"long", "0123456789ABCDEF0123456789ABCDEF00", true},
		{"non_hex", "0123456789ABCDEF0123456789ABCDEG", true},
		{"whitespace", "0123456789ABCDEF0123456789ABCDE ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := validateSteamWebAPIKey(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Len(t, key, 32)
		})
	}
}
