package relay_test

import (
	"crypto/tls"
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
		// The end-to-end test relays between loopback clients, which the
		// default private-peer filter denies; opt in like a trusted self-host.
		AllowPrivatePeers: true,
		Issuer:            iss,
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

func newTURNStreamClient(t *testing.T, turnAddr string, creds *relay.Credentials, useTLS bool) *turn.Client {
	t.Helper()
	var (
		conn net.Conn
		err  error
	)
	if useTLS {
		conn, err = tls.Dial("tcp4", turnAddr, &tls.Config{ //nolint:gosec // ephemeral test certificate
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		})
	} else {
		conn, err = net.Dial("tcp4", turnAddr)
	}
	require.NoError(t, err)
	packetConn := turn.NewSTUNConn(conn)
	c, err := turn.NewClient(&turn.ClientConfig{
		TURNServerAddr: turnAddr,
		Conn:           packetConn,
		Username:       creds.Username,
		Password:       creds.Password,
		Realm:          creds.Realm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	require.NoError(t, err)
	require.NoError(t, c.Listen())
	t.Cleanup(func() {
		c.Close()
		_ = packetConn.Close()
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

// A forged request can copy a real username and deliberately send bad
// MESSAGE-INTEGRITY. It must be rejected before the authenticated subject's
// allocation budget is charged.
func TestRelayBadIntegrityCannotDrainVictimLimiter(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:             "127.0.0.1",
		BindAddr:             "127.0.0.1",
		BindPort:             port,
		PlayerAllocPerMinute: 6,
		PlayerAllocBurst:     1,
		Issuer:               iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	victim, err := iss.Issue(1, 42)
	require.NoError(t, err)
	forged := *victim
	forged.Password = "tampered-password"

	attacker := newTURNClient(t, addr, &forged)
	_, err = attacker.Allocate()
	require.Error(t, err)
	assert.Zero(t, srv.AllocThrottled(), "bad integrity never reaches the limiter")

	client := newTURNClient(t, addr, victim)
	allocation, err := client.Allocate()
	require.NoError(t, err, "the forged request did not spend the victim's token")
	require.NoError(t, allocation.Close())
	assert.Zero(t, srv.AllocThrottled())
}

func TestRelayBadIntegrityCannotDrainVictimLimiterOverStreams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		useTLS bool
	}{
		{name: "tcp"},
		{name: "tls", useTLS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamPort := unusedTCPPort(t)
			iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
			cfg := relay.ServerConfig{
				PublicIP:             "127.0.0.1",
				BindAddr:             "127.0.0.1",
				BindPort:             unusedUDPPort(t),
				PlayerAllocPerMinute: 6,
				PlayerAllocBurst:     1,
				Issuer:               iss,
			}
			if tc.useTLS {
				cfg.TLSPort = streamPort
				cfg.TLSCertFile, cfg.TLSKeyFile = writeSelfSignedCert(t)
			} else {
				cfg.TCPPort = streamPort
			}
			srv, err := relay.NewServer(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { _ = srv.Close() })
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(streamPort))

			victim, err := iss.Issue(1, 42)
			require.NoError(t, err)
			forged := *victim
			forged.Password = "tampered-password"

			attacker := newTURNStreamClient(t, addr, &forged, tc.useTLS)
			_, err = attacker.Allocate()
			require.Error(t, err)
			assert.Zero(t, srv.AllocThrottled(), "bad integrity never reaches the limiter")

			client := newTURNStreamClient(t, addr, victim, tc.useTLS)
			allocation, err := client.Allocate()
			require.NoError(t, err, "the forged request did not spend the victim's token")
			require.NoError(t, allocation.Close())
			assert.Zero(t, srv.AllocThrottled())
		})
	}
}

func TestRelayAuthenticatedAllocationsReachPlayerLimiter(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:             "127.0.0.1",
		BindAddr:             "127.0.0.1",
		BindPort:             port,
		PlayerAllocPerMinute: 6,
		PlayerAllocBurst:     1,
		Issuer:               iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	first := newTURNClient(t, addr, creds)
	allocation, err := first.Allocate()
	require.NoError(t, err)
	t.Cleanup(func() { _ = allocation.Close() })

	second := newTURNClient(t, addr, creds)
	_, err = second.Allocate()
	require.Error(t, err, "the second authenticated allocation exhausts burst one")
	assert.Positive(t, srv.AllocThrottled(), "post-authentication refusals are counted")
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
