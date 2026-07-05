package players

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/remoteaddr"
)

func TestRemoteAddrsFromForm_parses_typed_rows_into_slots(t *testing.T) {
	form := url.Values{
		"addr_type_1":  {"ip"},
		"addr_value_1": {"192.168.1.4:9000"},
		"addr_type_2":  {"ip"},
		"addr_value_2": {"203.0.113.9"},
		"addr_type_3":  {"dns"},
		"addr_value_3": {"nas.local"},
		"addr_type_4":  {"iroh"},
		"addr_value_4": {strings.Repeat("ab", 32)},
	}

	set, err := remoteAddrsFromForm(form)

	require.NoError(t, err)
	require.NotNil(t, set.IPLAN)
	assert.Equal(t, "192.168.1.4:9000", set.IPLAN.Value)
	require.NotNil(t, set.IPPublic)
	assert.Equal(t, "203.0.113.9", set.IPPublic.Value)
	require.NotNil(t, set.DNS)
	assert.Equal(t, remoteaddr.ScopeLAN, set.DNS.Scope)
	require.NotNil(t, set.Iroh)
}

func TestRemoteAddrsFromForm_skips_rows_with_empty_values(t *testing.T) {
	form := url.Values{
		"addr_type_1":  {"ip"},
		"addr_value_1": {"  "},
		"addr_type_2":  {"dns"},
		"addr_value_2": {"example.com"},
	}

	set, err := remoteAddrsFromForm(form)

	require.NoError(t, err)
	assert.Nil(t, set.IPLAN)
	assert.Nil(t, set.IPPublic)
	require.NotNil(t, set.DNS)
	assert.Equal(t, "example.com", set.DNS.Value)
}

func TestRemoteAddrsFromForm_rejects_invalid_value_with_row_number(t *testing.T) {
	form := url.Values{
		"addr_type_2":  {"ip"},
		"addr_value_2": {"not-an-ip"},
	}

	_, err := remoteAddrsFromForm(form)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "row 2")
	assert.Contains(t, err.Error(), "not a valid IP address")
}

func TestRemoteAddrsFromForm_rejects_duplicate_slot(t *testing.T) {
	form := url.Values{
		"addr_type_1":  {"ip"},
		"addr_value_1": {"203.0.113.9"},
		"addr_type_2":  {"ip"},
		"addr_value_2": {"198.51.100.7"},
	}

	_, err := remoteAddrsFromForm(form)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "public IP address is already set")
}

func TestRemoteAddrsFromForm_rejects_unknown_type(t *testing.T) {
	form := url.Values{
		"addr_type_1":  {"tailscale"},
		"addr_value_1": {"whatever"},
	}

	_, err := remoteAddrsFromForm(form)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "row 1")
}

func TestAccountHomePage_renders_typed_remote_addr_rows(t *testing.T) {
	vm := AccountHomeView{
		Email: "p@example.com",
		RemoteAddrRows: []RemoteAddrRowView{
			{TypeValue: "ip", Value: "192.168.1.4:9000", ScopeLabel: "LAN only"},
			{TypeValue: "dns", Value: "example.com", ScopeLabel: ""},
			{TypeValue: "ip"},
			{TypeValue: "ip"},
		},
	}
	var sb strings.Builder

	err := AccountHomePage(vm).Render(context.Background(), &sb)

	require.NoError(t, err)
	html := sb.String()
	assert.Contains(t, html, `name="addr_type_1"`)
	assert.Contains(t, html, `name="addr_value_4"`)
	assert.Contains(t, html, `value="192.168.1.4:9000"`)
	assert.Contains(t, html, "LAN only")
	assert.Contains(t, html, ">IP address</option>")
	assert.Contains(t, html, ">DNS name</option>")
	assert.Contains(t, html, ">Iroh Endpoint ID</option>")
}
