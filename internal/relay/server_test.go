package relay_test

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/relay"
)

func TestServerCloseIgnoresListenerAlreadyClosedByTurn(t *testing.T) {
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: unusedUDPPort(t),
		Issuer:   relay.NewIssuer("shared-secret", "ggscale", time.Minute),
	})
	require.NoError(t, err)

	require.NoError(t, srv.Close())
}

func unusedUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).Port
}
