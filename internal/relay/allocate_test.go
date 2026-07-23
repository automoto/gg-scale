package relay_test

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/relay"
)

// startTestRelay boots a real TURN server on a loopback UDP port and returns
// its dial address plus the issuer that mints credentials for it.
func startTestRelay(t *testing.T, secret string) (addr string, iss *relay.Issuer) {
	t.Helper()
	port := unusedUDPPort(t)
	iss = relay.NewIssuer(secret, "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: port,
		Issuer:   iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), iss
}

func newTURNClient(t *testing.T, turnAddr string, creds *relay.Credentials) *turn.Client {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	c, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: turnAddr,
		Conn:           conn,
		Username:       creds.Username,
		Password:       creds.Password,
		Realm:          creds.Realm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)
	require.NoError(t, c.Listen())
	t.Cleanup(func() {
		c.Close()
		_ = conn.Close()
	})
	return c
}

// TestRelayEndToEndPacket exercises the full path: two clients authenticate
// with issuer-minted credentials, each allocates a relay address on the TURN
// server, and a datagram sent from one is forwarded through the relay to the
// other. This is the behavioral coverage the packet-relay path previously
// lacked.
func TestRelayEndToEndPacket(t *testing.T) {
	addr, iss := startTestRelay(t, strings.Repeat("s", 32))

	credsA, err := iss.Issue(1, 100)
	require.NoError(t, err)
	credsB, err := iss.Issue(1, 200)
	require.NoError(t, err)

	clientA := newTURNClient(t, addr, credsA)
	clientB := newTURNClient(t, addr, credsB)

	relayA, err := clientA.Allocate()
	require.NoError(t, err)
	relayB, err := clientB.Allocate()
	require.NoError(t, err)

	// Each side must permit the other's relayed address before the server
	// forwards datagrams to it.
	require.NoError(t, clientA.CreatePermission(relayB.LocalAddr()))
	require.NoError(t, clientB.CreatePermission(relayA.LocalAddr()))

	payload := []byte("hello via relay")
	_, err = relayA.WriteTo(payload, relayB.LocalAddr())
	require.NoError(t, err)

	buf := make([]byte, 1500)
	require.NoError(t, relayB.SetReadDeadline(time.Now().Add(3*time.Second)))
	n, from, err := relayB.ReadFrom(buf)
	require.NoError(t, err)
	assert.Equal(t, payload, buf[:n])
	assert.Equal(t, relayA.LocalAddr().String(), from.String())
}

// TestRelayAllocatesWithinPortRange confirms a bounded relay port range is
// honoured: the relayed address the server hands back falls inside [min,max].
func TestRelayAllocatesWithinPortRange(t *testing.T) {
	const minPort, maxPort = 50100, 50130
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:     "127.0.0.1",
		BindAddr:     "127.0.0.1",
		BindPort:     port,
		RelayMinPort: minPort,
		RelayMaxPort: maxPort,
		Issuer:       iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	client := newTURNClient(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), creds)

	relayConn, err := client.Allocate()
	require.NoError(t, err)
	_, portStr, err := net.SplitHostPort(relayConn.LocalAddr().String())
	require.NoError(t, err)
	got, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, got, minPort)
	assert.LessOrEqual(t, got, maxPort)
}

// TestRelayRejectsBadPassword confirms the AuthHandler refuses an allocation
// whose password does not match the issuer's HMAC.
func TestRelayRejectsBadPassword(t *testing.T) {
	addr, iss := startTestRelay(t, strings.Repeat("s", 32))
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	creds.Password = "tampered-password"

	client := newTURNClient(t, addr, creds)
	_, err = client.Allocate()
	require.Error(t, err)
}

// TestRelayAllocationCapEnforced confirms MaxAllocations bounds concurrent
// allocations node-wide: with a cap of 1, the first allocation succeeds and
// moves the gauge, the second is refused and counted as a rejection.
func TestRelayAllocationCapEnforced(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:       "127.0.0.1",
		BindAddr:       "127.0.0.1",
		BindPort:       port,
		MaxAllocations: 1,
		Issuer:         iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	credsA, err := iss.Issue(1, 100)
	require.NoError(t, err)
	credsB, err := iss.Issue(1, 200)
	require.NoError(t, err)

	clientA := newTURNClient(t, addr, credsA)
	_, err = clientA.Allocate()
	require.NoError(t, err)
	assert.Equal(t, int64(1), srv.ActiveAllocations())

	clientB := newTURNClient(t, addr, credsB)
	_, err = clientB.Allocate()
	require.Error(t, err, "second allocation must be refused by the cap")
	assert.GreaterOrEqual(t, srv.RejectedAllocations(), int64(1))
}
