package relay_test

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/relay"
)

const drainTestSecret = "drain-test-shared-secret-32-bytes!"

var _ lifecycle.Drainable = (*relay.Server)(nil)

func startDrainableRelay(t *testing.T) (addr string, iss *relay.Issuer, srv *relay.Server) {
	t.Helper()
	port := unusedUDPPort(t)
	iss = relay.NewIssuer(drainTestSecret, "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP:          "127.0.0.1",
		BindAddr:          "127.0.0.1",
		BindPort:          port,
		AllowPrivatePeers: true,
		Issuer:            iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), iss, srv
}

func TestRelayDrain_should_refuse_new_allocate(t *testing.T) {
	addr, iss, srv := startDrainableRelay(t)
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	client := newTURNClient(t, addr, creds)

	srv.BeginDrain()
	_, err = client.Allocate()

	assert.Error(t, err)
}

func TestRelayDrain_should_count_refused_allocate(t *testing.T) {
	addr, iss, srv := startDrainableRelay(t)
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	client := newTURNClient(t, addr, creds)
	srv.BeginDrain()

	_, _ = client.Allocate()

	assert.GreaterOrEqual(t, srv.RefusedWork(), int64(1))
}

func TestRelayDrain_should_keep_existing_allocation_relaying_data(t *testing.T) {
	addr, iss, srv := startDrainableRelay(t)
	credsA, err := iss.Issue(1, 100)
	require.NoError(t, err)
	credsB, err := iss.Issue(1, 200)
	require.NoError(t, err)
	relayA, err := newTURNClient(t, addr, credsA).Allocate()
	require.NoError(t, err)
	clientB := newTURNClient(t, addr, credsB)
	relayB, err := clientB.Allocate()
	require.NoError(t, err)

	srv.BeginDrain()
	require.NoError(t, clientB.CreatePermission(relayA.LocalAddr()))
	_, err = relayA.WriteTo([]byte("still here"), relayB.LocalAddr())
	require.NoError(t, err)

	buf := make([]byte, 1500)
	require.NoError(t, relayB.SetReadDeadline(time.Now().Add(3*time.Second)))
	n, _, err := relayB.ReadFrom(buf)
	require.NoError(t, err)
	assert.Equal(t, "still here", string(buf[:n]))
}

func TestRelayDrain_should_keep_refreshing_existing_allocation(t *testing.T) {
	addr, _, srv := startDrainableRelay(t)
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	dst, err := net.ResolveUDPAddr("udp4", addr)
	require.NoError(t, err)
	creds := credentialsExpiringAt(drainTestSecret, time.Now().Add(5*time.Minute), 7, 42)
	realm, nonce := authenticatedAllocate(t, conn, dst, creds)

	srv.BeginDrain()
	response := transactSTUN(t, conn, dst, authenticatedRequest(stun.MethodRefresh, creds, realm, nonce))

	assert.Equal(t, stun.ClassSuccessResponse, response.Type.Class)
}

func TestRelayDrain_should_allocate_again_after_resume(t *testing.T) {
	addr, iss, srv := startDrainableRelay(t)
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)
	srv.BeginDrain()
	srv.EndDrain()

	_, err = newTURNClient(t, addr, creds).Allocate()

	assert.NoError(t, err)
}

func TestRelayActiveWork_should_count_live_allocations(t *testing.T) {
	addr, iss, srv := startDrainableRelay(t)
	creds, err := iss.Issue(1, 100)
	require.NoError(t, err)

	_, err = newTURNClient(t, addr, creds).Allocate()
	require.NoError(t, err)

	assert.Equal(t, int64(1), srv.ActiveWork())
}

func TestRelayTransports_should_list_enabled_listeners(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: unusedUDPPort(t),
		TCPPort:  unusedTCPPort(t),
		Issuer:   iss,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	assert.Equal(t, []string{"udp", "tcp"}, srv.Transports())
}
