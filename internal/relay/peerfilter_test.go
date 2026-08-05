package relay

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPrivatePeer(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		private bool
	}{
		{"loopback", "127.0.0.1", true},
		{"rfc1918 10", "10.1.2.3", true},
		{"rfc1918 172", "172.16.5.4", true},
		{"rfc1918 192", "192.168.0.1", true},
		{"link-local", "169.254.1.1", true},
		{"unspecified", "0.0.0.0", true},
		{"loopback v6", "::1", true},
		{"link-local v6", "fe80::1", true},
		{"ula", "fc00::1", true},

		// Carrier-grade NAT. Also the range Tailscale assigns from, so the
		// relay node's own tailnet lives here.
		{"cgnat low", "100.64.0.1", true},
		{"cgnat high", "100.127.255.254", true},
		{"below cgnat", "100.63.255.255", false},
		{"above cgnat", "100.128.0.0", false},

		// Multicast and broadcast are not unicast peers at all.
		{"admin-scoped multicast", "239.1.2.3", true},
		{"internetwork multicast", "224.0.1.1", true},
		{"limited broadcast", "255.255.255.255", true},
		{"site-local multicast v6", "ff05::1", true},

		// The IPv4-mapped forms must be classified the same way.
		{"mapped rfc1918", "::ffff:10.0.0.1", true},
		{"mapped cgnat", "::ffff:100.64.0.1", true},
		{"mapped public", "::ffff:8.8.8.8", false},

		{"public v4", "8.8.8.8", false},
		{"public v4 2", "1.1.1.1", false},
		{"public v4 3", "203.0.113.10", false},
		{"public v6", "2606:4700:4700::1111", false},
		{"public v6 2", "2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip := net.ParseIP(c.ip)
			assert.NotNil(t, ip, "test case parses")
			assert.Equal(t, c.private, isPrivatePeer(ip), "isPrivatePeer(%s)", c.ip)
		})
	}
}

func TestPermissionHandlerDeniesPrivatePeersByDefault(t *testing.T) {
	s := &Server{}
	deny := s.permissionHandler(false)

	assert.False(t, deny(nil, net.ParseIP("10.0.0.1")), "private peer denied")
	assert.False(t, deny(nil, net.ParseIP("127.0.0.1")), "loopback denied")
	assert.False(t, deny(nil, net.ParseIP("100.64.0.1")), "cgnat peer denied")
	assert.False(t, deny(nil, net.ParseIP("239.1.2.3")), "multicast peer denied")
	assert.False(t, deny(nil, net.ParseIP("255.255.255.255")), "broadcast peer denied")
	assert.True(t, deny(nil, net.ParseIP("8.8.8.8")), "public peer allowed")
	assert.Equal(t, int64(5), s.PeerRejected(), "each denied peer is counted")
}

func TestPermissionHandlerAllowPrivateAdmitsEverything(t *testing.T) {
	s := &Server{}
	allow := s.permissionHandler(true)

	assert.True(t, allow(nil, net.ParseIP("10.0.0.1")))
	assert.True(t, allow(nil, net.ParseIP("127.0.0.1")))
	assert.Equal(t, int64(0), s.PeerRejected(), "nothing rejected when private peers are allowed")
}
