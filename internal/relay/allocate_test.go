package relay_test

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by TURN long-term credentials
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relay"
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
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:          "127.0.0.1",
		BindAddr:          "127.0.0.1",
		BindPort:          port,
		AllowPrivatePeers: true,
		Issuer:            iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	creds.Password = "tampered-password"

	client := newTURNClient(t, addr, creds)
	_, err = client.Allocate()
	require.Error(t, err)
	assert.Equal(t, int64(1), srv.AuthFailures(), "MESSAGE-INTEGRITY failures are observable")
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

func TestRelayRejectsTCPAllocationWithoutWedgingUDP(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:          "127.0.0.1",
		BindAddr:          "127.0.0.1",
		BindPort:          port,
		AllowPrivatePeers: true,
		Issuer:            iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	tcpCreds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	tcpClient := newTURNClient(t, addr, tcpCreds)
	_, err = tcpClient.AllocateTCP()
	require.Error(t, err, "RFC 6062 allocations are not supported")

	udpCreds, err := iss.Issue(2, 99)
	require.NoError(t, err)
	udpClient := newTURNClient(t, addr, udpCreds)
	allocation, err := udpClient.Allocate()
	require.NoError(t, err, "a refused TCP allocation leaves the shared manager responsive")
	require.NoError(t, allocation.Close())
}

func TestRelayRejectsConnectWithoutDialingPeer(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:          "127.0.0.1",
		BindAddr:          "127.0.0.1",
		BindPort:          port,
		AllowPrivatePeers: true,
		Issuer:            iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dst, err := net.ResolveUDPAddr("udp4", addr)
	require.NoError(t, err)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	realm, nonce := authenticatedAllocate(t, conn, dst, creds)
	peer, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	response := transactSTUN(t, conn, dst, authenticatedRequest(
		stun.MethodConnect,
		creds,
		realm,
		nonce,
		xorPeerAddress{IP: peer.Addr().(*net.TCPAddr).IP, Port: peer.Addr().(*net.TCPAddr).Port},
	))
	var errorCode stun.ErrorCodeAttribute
	require.NoError(t, errorCode.GetFrom(response))
	assert.Equal(t, stun.CodeConnTimeoutOrFailure, errorCode.Code)

	require.NoError(t, peer.SetDeadline(time.Now().Add(100*time.Millisecond)))
	accepted, err := peer.Accept()
	if accepted != nil {
		_ = accepted.Close()
	}
	assert.Error(t, err, "the relay never opens an outbound TCP socket")
}

func TestRelayRenewedCredentialControlsExistingAllocation(t *testing.T) {
	const secret = "renewal-test-shared-secret-32-bytes"
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(secret, "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:          "127.0.0.1",
		BindAddr:          "127.0.0.1",
		BindPort:          port,
		AllowPrivatePeers: true,
		Issuer:            iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	dst, err := net.ResolveUDPAddr("udp4", addr)
	require.NoError(t, err)

	oldCreds := credentialsExpiringAt(secret, time.Now().Add(5*time.Minute), 7, 42)
	renewedCreds := credentialsExpiringAt(secret, time.Now().Add(10*time.Minute), 7, 42)
	require.NotEqual(t, oldCreds.Username, renewedCreds.Username)
	realm, nonce := authenticatedAllocate(t, conn, dst, oldCreds)

	refresh := authenticatedRequest(
		stun.MethodRefresh,
		renewedCreds,
		realm,
		nonce,
	)
	refreshResponse := transactSTUN(t, conn, dst, refresh)
	assert.Equal(t, stun.ClassSuccessResponse, refreshResponse.Type.Class)

	permission := authenticatedRequest(
		stun.MethodCreatePermission,
		renewedCreds,
		realm,
		nonce,
		xorPeerAddress{IP: net.ParseIP("127.0.0.1"), Port: 9999},
	)
	permissionResponse := transactSTUN(t, conn, dst, permission)
	assert.Equal(t, stun.ClassSuccessResponse, permissionResponse.Type.Class)
}

func TestRelayRejectsEvenPortBeforeNodeAllocator(t *testing.T) {
	port := unusedUDPPort(t)
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:             "127.0.0.1",
		BindAddr:             "127.0.0.1",
		BindPort:             port,
		MaxAllocations:       1,
		PlayerAllocPerMinute: 6,
		PlayerAllocBurst:     1,
		Issuer:               iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	client := newTURNClient(t, addr, creds)
	allocation, err := client.Allocate()
	require.NoError(t, err)
	t.Cleanup(func() { _ = allocation.Close() })

	response := sendEvenPortAllocate(t, addr, creds)
	var errorCode stun.ErrorCodeAttribute
	require.NoError(t, errorCode.GetFrom(response))

	assert.Equal(t, stun.CodeInsufficientCapacity, errorCode.Code)
	assert.Zero(t, srv.AllocThrottled(), "unsupported EVEN-PORT is not reported as player abuse")
	assert.Zero(t, srv.RejectedAllocations(), "unsupported EVEN-PORT never reaches the node allocator")
}

type requestedUDPTransport struct{}

func (requestedUDPTransport) AddTo(m *stun.Message) error {
	m.Add(stun.AttrRequestedTransport, []byte{17, 0, 0, 0})
	return nil
}

type reserveEvenPort struct{}

func (reserveEvenPort) AddTo(m *stun.Message) error {
	m.Add(stun.AttrEvenPort, []byte{0xff})
	return nil
}

type xorPeerAddress stun.XORMappedAddress

func (a xorPeerAddress) AddTo(m *stun.Message) error {
	return stun.XORMappedAddress(a).AddToAs(m, stun.AttrXORPeerAddress)
}

func credentialsExpiringAt(secret string, expires time.Time, tenantID, playerID int64) *relay.Credentials {
	kidHash := sha256.Sum256([]byte(secret))
	kid := hex.EncodeToString(kidHash[:4])
	username := fmt.Sprintf("%d:%d:%d:%s", expires.Unix(), tenantID, playerID, kid)
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(username))
	return &relay.Credentials{
		Username: username,
		Password: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		Realm:    "ggscale",
	}
}

func authenticatedAllocate(
	t *testing.T,
	conn net.PacketConn,
	dst net.Addr,
	creds *relay.Credentials,
) (stun.Realm, stun.Nonce) {
	t.Helper()
	challenge := transactSTUN(t, conn, dst, stun.MustBuild(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassRequest),
		requestedUDPTransport{},
		stun.Fingerprint,
	))
	var nonce stun.Nonce
	require.NoError(t, nonce.GetFrom(challenge))
	var realm stun.Realm
	require.NoError(t, realm.GetFrom(challenge))
	response := transactSTUN(t, conn, dst, authenticatedRequest(
		stun.MethodAllocate,
		creds,
		realm,
		nonce,
		requestedUDPTransport{},
	))
	require.Equal(t, stun.ClassSuccessResponse, response.Type.Class)
	return realm, nonce
}

func authenticatedRequest(
	method stun.Method,
	creds *relay.Credentials,
	realm stun.Realm,
	nonce stun.Nonce,
	attributes ...stun.Setter,
) *stun.Message {
	setters := []stun.Setter{
		stun.TransactionID,
		stun.NewType(method, stun.ClassRequest),
	}
	setters = append(setters, attributes...)
	setters = append(setters,
		stun.NewUsername(creds.Username),
		&realm,
		&nonce,
		stun.NewLongTermIntegrity(creds.Username, realm.String(), creds.Password),
		stun.Fingerprint,
	)
	return stun.MustBuild(setters...)
}

func sendEvenPortAllocate(t *testing.T, turnAddr string, creds *relay.Credentials) *stun.Message {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	dst, err := net.ResolveUDPAddr("udp4", turnAddr)
	require.NoError(t, err)

	challenge := transactSTUN(t, conn, dst, stun.MustBuild(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassRequest),
		requestedUDPTransport{},
		reserveEvenPort{},
		stun.Fingerprint,
	))
	var nonce stun.Nonce
	require.NoError(t, nonce.GetFrom(challenge))
	var realm stun.Realm
	require.NoError(t, realm.GetFrom(challenge))
	integrity := stun.NewLongTermIntegrity(creds.Username, realm.String(), creds.Password)

	return transactSTUN(t, conn, dst, stun.MustBuild(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassRequest),
		requestedUDPTransport{},
		stun.NewUsername(creds.Username),
		&realm,
		&nonce,
		&integrity,
		reserveEvenPort{},
		stun.Fingerprint,
	))
}

func transactSTUN(t *testing.T, conn net.PacketConn, dst net.Addr, request *stun.Message) *stun.Message {
	t.Helper()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err := conn.WriteTo(request.Raw, dst)
	require.NoError(t, err)
	buf := make([]byte, 1600)
	n, _, err := conn.ReadFrom(buf)
	require.NoError(t, err)
	response := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
	require.NoError(t, response.Decode())
	return response
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
