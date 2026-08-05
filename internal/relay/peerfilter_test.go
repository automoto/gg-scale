package relay

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPrivatePeer(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.5.4", true},
		{"192.168.0.1", true},
		{"169.254.1.1", true}, // link-local
		{"0.0.0.0", true},     // unspecified
		{"::1", true},
		{"fe80::1", true}, // link-local v6
		{"fc00::1", true}, // ULA
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"203.0.113.10", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		assert.Equal(t, c.private, isPrivatePeer(ip), "isPrivatePeer(%s)", c.ip)
	}
}

func TestPermissionHandlerDeniesPrivatePeersByDefault(t *testing.T) {
	s := &Server{}
	deny := s.permissionHandler(false)

	assert.False(t, deny(nil, net.ParseIP("10.0.0.1")), "private peer denied")
	assert.False(t, deny(nil, net.ParseIP("127.0.0.1")), "loopback denied")
	assert.True(t, deny(nil, net.ParseIP("8.8.8.8")), "public peer allowed")
	assert.Equal(t, int64(2), s.PeerRejected(), "each denied peer is counted")
}

func TestPermissionHandlerAllowPrivateAdmitsEverything(t *testing.T) {
	s := &Server{}
	allow := s.permissionHandler(true)

	assert.True(t, allow(nil, net.ParseIP("10.0.0.1")))
	assert.True(t, allow(nil, net.ParseIP("127.0.0.1")))
	assert.Equal(t, int64(0), s.PeerRejected(), "nothing rejected when private peers are allowed")
}
