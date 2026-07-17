package players

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

func TestAccountHomePage_renders_signed_in_header(t *testing.T) {
	vm := AccountHomeView{Email: "p@example.com", CSRFToken: "tok"}
	var sb strings.Builder

	err := AccountHomePage(vm).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, `aria-label="Player account navigation"`)
	assert.Contains(t, html, `<summary>Account</summary>`)
	assert.Contains(t, html, `aria-current="page">p@example.com</a>`)
	assert.Contains(t, html, `href="/v1/players/account/friends">Friends</a>`)
	assert.Contains(t, html, `href="/v1/players/account/2fa">Security</a>`)
	assert.Equal(t, 0, strings.Count(html, `>Account</a>`))
	assert.Contains(t, html, `action="/v1/players/account/logout"`)
	assert.Contains(t, html, `<input type="hidden" name="_csrf" value="tok">`)
	assert.Contains(t, html, `rel="icon" type="image/svg+xml" href="/v1/assets/favicon.svg?v=`)
}

func TestFriendsPage_uses_header_navigation(t *testing.T) {
	var sb strings.Builder

	err := FriendsPage(FriendsView{AccountEmail: "p@example.com", CSRFToken: "tok"}).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, `aria-current="page">Friends</a>`)
	assert.Contains(t, html, `href="/v1/players/account/">p@example.com</a>`)
	assert.NotContains(t, html, "Back to account")
}

func TestConnectionAddressRows_maps_stored_slots(t *testing.T) {
	lan := "192.168.1.4:9000"
	dns := "nas.local"
	rows := connectionAddressRows(sqlcgen.GetPlayerAccountRemoteAddrsRow{
		RemoteAddrIpLan: &lan,
		RemoteAddrDns:   &dns,
	})

	require.Len(t, rows, 2)
	assert.Equal(t, "ip-lan", rows[0].Slot)
	assert.Equal(t, "LAN IP address", rows[0].TypeLabel)
	assert.Equal(t, "LAN only", rows[0].ScopeLabel)
	assert.Equal(t, "dns", rows[1].Slot)
	assert.Equal(t, "DNS name", rows[1].TypeLabel)
}

func TestAccountHomePage_shows_connection_address_summary_not_blank_inputs(t *testing.T) {
	vm := AccountHomeView{
		Email:           "p@example.com",
		CSRFToken:       "tok",
		RemoteAddrCount: 2,
	}
	var sb strings.Builder

	err := AccountHomePage(vm).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, "Connection addresses")
	assert.Contains(t, html, "2 configured")
	assert.Contains(t, html, `/v1/players/account/remote-addrs`)
	assert.Contains(t, html, "Manage addresses")
	assert.NotContains(t, html, `name="addr_type_1"`)
	assert.NotContains(t, html, `name="addr_value_4"`)
}

func TestAccountHomePage_does_not_duplicate_nav_or_render_public_join_form(t *testing.T) {
	vm := AccountHomeView{
		Email:     "p@example.com",
		CSRFToken: "tok",
		Projects:  []LinkedProject{{ProjectName: "Abyssal Depths", ExternalID: "player-1"}},
	}
	var sb strings.Builder

	err := AccountHomePage(vm).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, "Abyssal Depths")
	assert.Contains(t, html, "player-1")
	assert.NotContains(t, html, "Manage friends")
	assert.NotContains(t, html, "Two-factor authentication")
	assert.NotContains(t, html, "Join a game")
	assert.NotContains(t, html, `action="/v1/players/account/join"`)
	assert.NotContains(t, html, `name="project_id"`)
}

func TestConnectionAddressesPage_renders_empty_state_and_add_action(t *testing.T) {
	var sb strings.Builder

	err := ConnectionAddressesPage(ConnectionAddressesView{CSRFToken: "tok", CanAdd: true}).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, "No connection addresses added")
	assert.Contains(t, html, `/v1/players/account/remote-addrs/new`)
}

func TestConnectionAddressesPage_remove_is_a_link_not_a_js_confirm(t *testing.T) {
	var sb strings.Builder

	err := ConnectionAddressesPage(ConnectionAddressesView{
		CSRFToken: "tok",
		Addresses: []ConnectionAddressView{{Slot: "dns", TypeLabel: "DNS name", Value: "nas.local"}},
	}).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	// The player UI ships no JS, so deletion must go through a confirmation
	// page (a GET link), never a data-confirm dialog that silently no-ops.
	assert.NotContains(t, html, "data-confirm")
	assert.Contains(t, html, `href="/v1/players/account/remote-addrs/dns/delete"`)
}

func TestConnectionAddressDeletePage_renders_confirm_form(t *testing.T) {
	var sb strings.Builder

	err := ConnectionAddressDeletePage(ConnectionAddressDeleteView{
		CSRFToken: "tok",
		Slot:      "dns",
		TypeLabel: "DNS name",
		Value:     "nas.local",
	}).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, `action="/v1/players/account/remote-addrs/dns/delete"`)
	assert.Contains(t, html, "nas.local")
	assert.Contains(t, html, "Remove address")
	assert.Contains(t, html, `<input type="hidden" name="_csrf" value="tok">`)
}

func TestConnectionAddressFormPage_renders_edit_action(t *testing.T) {
	var sb strings.Builder

	err := ConnectionAddressFormPage(ConnectionAddressFormView{
		CSRFToken: "tok",
		Slot:      "dns",
		TypeValue: "dns",
		Value:     "nas.local",
		Editing:   true,
	}).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, `action="/v1/players/account/remote-addrs/dns"`)
	assert.Contains(t, html, `value="nas.local"`)
	assert.Contains(t, html, `<option value="dns" selected>DNS name</option>`)
}
