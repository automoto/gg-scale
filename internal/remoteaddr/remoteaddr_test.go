package remoteaddr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse_ip_classifies_private_ranges_as_lan(t *testing.T) {
	values := []string{
		"192.168.1.10",
		"10.0.0.1",
		"172.16.5.5",
		"127.0.0.1",
		"169.254.1.1",
		"fd12::1",
		"100.64.0.1",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			addr, err := Parse(TypeIP, v)

			assert.NoError(t, err)
			assert.Equal(t, ScopeLAN, addr.Scope)
		})
	}
}

func TestParse_ip_classifies_public_as_public(t *testing.T) {
	values := []string{"203.0.113.9", "2001:db8::1", "8.8.8.8"}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			addr, err := Parse(TypeIP, v)

			assert.NoError(t, err)
			assert.Equal(t, ScopePublic, addr.Scope)
		})
	}
}

func TestParse_ip_accepts_and_normalizes_ports(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"1.2.3.4:7777", "1.2.3.4:7777"},
		{"[2001:DB8::1]:7777", "[2001:db8::1]:7777"},
		{"192.168.1.10:65535", "192.168.1.10:65535"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			addr, err := Parse(TypeIP, tc.in)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, addr.Value)
		})
	}
}

func TestParse_ip_requires_brackets_for_ipv6_with_port(t *testing.T) {
	// Without brackets the trailing group is part of the address, not a port.
	addr, err := Parse(TypeIP, "2001:db8::1:7777")

	assert.NoError(t, err)
	assert.Equal(t, "2001:db8::1:7777", addr.Value)
}

func TestParse_ip_rejects_zone_unspecified_multicast_and_port_zero(t *testing.T) {
	values := []string{"fe80::1%eth0", "0.0.0.0", "::", "224.0.0.1", "1.2.3.4:0"}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			_, err := Parse(TypeIP, v)

			assert.Error(t, err)
		})
	}
}

func TestParse_ip_unmaps_ipv4_in_ipv6(t *testing.T) {
	addr, err := Parse(TypeIP, "::ffff:192.168.1.1")

	assert.NoError(t, err)
	assert.Equal(t, "192.168.1.1", addr.Value)
	assert.Equal(t, ScopeLAN, addr.Scope)
}

func TestParse_ip_rejects_garbage(t *testing.T) {
	values := []string{"", "not-an-ip", "999.1.1.1", "1.2.3.4:99999"}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			_, err := Parse(TypeIP, v)

			assert.Error(t, err)
		})
	}
}

func TestParse_dns_accepts_rfc1123_hostnames_and_ports(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Play.Example.COM:7777", "play.example.com:7777"},
		{"gamingpc", "gamingpc"},
		{"my-host.duckdns.org", "my-host.duckdns.org"},
		{"example.com:1", "example.com:1"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			addr, err := Parse(TypeDNS, tc.in)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, addr.Value)
		})
	}
}

func TestParse_dns_labels_lan_suffixes(t *testing.T) {
	tests := []struct {
		in   string
		want Scope
	}{
		{"nas.local", ScopeLAN},
		{"game.internal", ScopeLAN},
		{"pc.lan", ScopeLAN},
		{"x.home.arpa", ScopeLAN},
		{"nas.local:7777", ScopeLAN},
		{"example.com", ScopeNone},
		{"gamingpc", ScopeNone},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			addr, err := Parse(TypeDNS, tc.in)

			assert.NoError(t, err)
			assert.Equal(t, tc.want, addr.Scope)
		})
	}
}

func TestParse_dns_rejects_invalid_hostnames(t *testing.T) {
	values := []string{
		"",
		"host..example.com",
		strings.Repeat("a", 64) + ".com",
		"-bad.example.com",
		"bad-.example.com",
		"under_score.example.com",
		strings.Repeat("a.", 127) + "com",
		"example.com.",
		"example.com:0",
		"example.com:70000",
		"example.com:abc",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			_, err := Parse(TypeDNS, v)

			assert.Error(t, err)
		})
	}
}

func TestParse_dns_rejects_ip_literals(t *testing.T) {
	values := []string{"1.2.3.4", "[::1]:80", "1.2.3.4:7777"}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			_, err := Parse(TypeDNS, v)

			assert.Error(t, err)
		})
	}
}

func TestParse_iroh_accepts_64_hex_and_normalizes_lowercase(t *testing.T) {
	id := strings.Repeat("Ab", 32)

	addr, err := Parse(TypeIroh, id)

	assert.NoError(t, err)
	assert.Equal(t, strings.ToLower(id), addr.Value)
	assert.Equal(t, ScopeNone, addr.Scope)
}

func TestParse_iroh_rejects_wrong_length_nonhex_and_ports(t *testing.T) {
	values := []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("g", 64),
		strings.Repeat("a", 64) + ":7777",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			_, err := Parse(TypeIroh, v)

			assert.Error(t, err)
		})
	}
}

func TestParse_trims_whitespace_and_rejects_overlong_input(t *testing.T) {
	addr, err := Parse(TypeIP, "  192.168.1.10 \n")
	assert.NoError(t, err)
	assert.Equal(t, "192.168.1.10", addr.Value)

	_, err = Parse(TypeDNS, strings.Repeat("a", 256))
	assert.Error(t, err)
}

func TestParseType_maps_strings_and_rejects_unknown(t *testing.T) {
	tests := []struct {
		in   string
		want Type
		ok   bool
	}{
		{"ip", TypeIP, true},
		{"dns", TypeDNS, true},
		{"iroh", TypeIroh, true},
		{"tailscale", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseType(tc.in)

			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAddress_slot_assignment(t *testing.T) {
	lan, _ := Parse(TypeIP, "192.168.1.10")
	pub, _ := Parse(TypeIP, "203.0.113.9")
	dns, _ := Parse(TypeDNS, "example.com")
	iroh, _ := Parse(TypeIroh, strings.Repeat("a", 64))

	assert.Equal(t, SlotIPLAN, lan.Slot())
	assert.Equal(t, SlotIPPublic, pub.Slot())
	assert.Equal(t, SlotDNS, dns.Slot())
	assert.Equal(t, SlotIroh, iroh.Slot())
}

func TestNewSet_allows_lan_and_public_ip_together(t *testing.T) {
	lan, _ := Parse(TypeIP, "192.168.1.10:7777")
	pub, _ := Parse(TypeIP, "203.0.113.9:7777")
	dns, _ := Parse(TypeDNS, "example.com")
	iroh, _ := Parse(TypeIroh, strings.Repeat("a", 64))

	set, err := NewSet([]Address{iroh, pub, lan, dns})

	assert.NoError(t, err)
	assert.Equal(t, []Address{lan, pub, dns, iroh}, set.List())
}

func TestNewSet_rejects_duplicate_slot(t *testing.T) {
	pub1, _ := Parse(TypeIP, "203.0.113.9")
	pub2, _ := Parse(TypeIP, "198.51.100.7")
	dns1, _ := Parse(TypeDNS, "a.example.com")
	dns2, _ := Parse(TypeDNS, "b.example.com")

	_, err := NewSet([]Address{pub1, pub2})
	assert.ErrorContains(t, err, "public IP address is already set")

	_, err = NewSet([]Address{dns1, dns2})
	assert.ErrorContains(t, err, "DNS name is already set")
}

func TestFromStored_rederives_dns_lan_scope(t *testing.T) {
	tests := []struct {
		slot  Slot
		value string
		want  Scope
	}{
		{SlotDNS, "nas.local:7777", ScopeLAN},
		{SlotDNS, "example.com", ScopeNone},
		{SlotIPLAN, "192.168.1.10", ScopeLAN},
		{SlotIPPublic, "203.0.113.9:7777", ScopePublic},
		{SlotIroh, strings.Repeat("a", 64), ScopeNone},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			addr := FromStored(tc.slot, tc.value)

			assert.Equal(t, tc.want, addr.Scope)
		})
	}
}

func TestSetFromValues_rebuilds_slots_in_order(t *testing.T) {
	lan := "192.168.1.4"
	dns := "nas.local:7777"

	set := SetFromValues(&lan, nil, &dns, nil)

	list := set.List()
	assert.Len(t, list, 2)
	assert.Equal(t, ScopeLAN, list[0].Scope)
	assert.Equal(t, "192.168.1.4", list[0].Value)
	assert.Equal(t, TypeDNS, list[1].Type)
	assert.Equal(t, ScopeLAN, list[1].Scope)
}

func TestSlot_labels(t *testing.T) {
	assert.Equal(t, "LAN IP address", SlotIPLAN.Label())
	assert.Equal(t, "public IP address", SlotIPPublic.Label())
	assert.Equal(t, "DNS name", SlotDNS.Label())
	assert.Equal(t, "Iroh endpoint ID", SlotIroh.Label())
}
