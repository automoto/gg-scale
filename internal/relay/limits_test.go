package relay_test

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relay"
)

func startLimitedRelay(t *testing.T, cfg relay.ServerConfig) (addr string, srv *relay.Server) {
	t.Helper()
	port := unusedUDPPort(t)
	cfg.PublicIP = "127.0.0.1"
	cfg.BindAddr = "127.0.0.1"
	cfg.BindPort = port
	cfg.AllowPrivatePeers = true
	srv, err := relay.NewServer(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), srv
}

// requestedLifetime is a LIFETIME attribute, so a test allocation can expire
// within the test.
type requestedLifetime time.Duration

func (l requestedLifetime) AddTo(m *stun.Message) error {
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v, uint32(time.Duration(l)/time.Second))
	m.Add(stun.AttrLifetime, v)
	return nil
}

// rawAllocate sends one authenticated Allocate from a fresh socket and returns
// the response. The socket stays open so a successful allocation stays live.
func rawAllocate(t *testing.T, turnAddr string, creds *relay.Credentials, attrs ...stun.Setter) *stun.Message {
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
		stun.Fingerprint,
	))
	var nonce stun.Nonce
	require.NoError(t, nonce.GetFrom(challenge))
	var realm stun.Realm
	require.NoError(t, realm.GetFrom(challenge))
	return transactSTUN(t, conn, dst, authenticatedRequest(
		stun.MethodAllocate, creds, realm, nonce,
		append([]stun.Setter{requestedUDPTransport{}}, attrs...)...,
	))
}

func errorCode(t *testing.T, response *stun.Message) stun.ErrorCode {
	t.Helper()
	var code stun.ErrorCodeAttribute
	require.NoError(t, code.GetFrom(response), "expected an error response")
	return code.Code
}

func TestRelayPlayerCap_should_refuse_allocation_over_cap_with_quota_reached(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, _ := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerPlayer: 2, Issuer: iss})
	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, creds).Type.Class)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, creds).Type.Class)

	response := rawAllocate(t, addr, creds)

	assert.Equal(t, stun.CodeAllocQuotaReached, errorCode(t, response))
}

func TestRelayPlayerCap_should_count_refusals(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, srv := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerPlayer: 1, Issuer: iss})
	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, creds).Type.Class)

	rawAllocate(t, addr, creds)

	assert.Equal(t, int64(1), srv.PlayerCapRejected())
}

func TestRelayPlayerCap_should_free_slot_when_allocation_is_deleted(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, _ := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerPlayer: 1, Issuer: iss})
	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	client := newTURNClient(t, addr, creds)
	allocation, err := client.Allocate()
	require.NoError(t, err)
	require.Equal(t, stun.CodeAllocQuotaReached, errorCode(t, rawAllocate(t, addr, creds)))

	// Close sends Refresh with LIFETIME 0, which deletes the allocation.
	require.NoError(t, allocation.Close())

	assert.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, creds).Type.Class)
}

func TestRelayPlayerCap_should_free_slot_when_allocation_expires(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, srv := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerPlayer: 1, Issuer: iss})
	creds, err := iss.Issue(1, 42)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse,
		rawAllocate(t, addr, creds, requestedLifetime(time.Second)).Type.Class)
	require.Equal(t, stun.CodeAllocQuotaReached, errorCode(t, rawAllocate(t, addr, creds)))

	require.Eventually(t, func() bool { return srv.ActiveAllocations() == 0 }, 5*time.Second, 50*time.Millisecond)

	assert.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, creds).Type.Class)
}

func TestRelayTenantCap_should_use_cap_from_credential(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	addr, _ := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerTenant: 2000, Issuer: iss})
	first, err := iss.IssueWithTenantCap(7, 1, 1)
	require.NoError(t, err)
	second, err := iss.IssueWithTenantCap(7, 2, 1)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, first).Type.Class)

	response := rawAllocate(t, addr, second)

	assert.Equal(t, stun.CodeAllocQuotaReached, errorCode(t, response))
}

func TestRelayTenantCap_should_use_default_for_credential_without_cap(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, srv := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerTenant: 1, Issuer: iss})
	first, err := iss.Issue(7, 1)
	require.NoError(t, err)
	second, err := iss.Issue(7, 2)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, first).Type.Class)

	rawAllocate(t, addr, second)

	assert.Equal(t, int64(1), srv.TenantCapRejected())
}

func TestRelayTenantCap_should_not_limit_other_tenants(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	addr, _ := startLimitedRelay(t, relay.ServerConfig{MaxAllocationsPerTenant: 1, Issuer: iss})
	first, err := iss.Issue(7, 1)
	require.NoError(t, err)
	other, err := iss.Issue(8, 1)
	require.NoError(t, err)
	require.Equal(t, stun.ClassSuccessResponse, rawAllocate(t, addr, first).Type.Class)

	response := rawAllocate(t, addr, other)

	assert.Equal(t, stun.ClassSuccessResponse, response.Type.Class)
}

func TestRelayBandwidth_should_drop_packets_over_allocation_rate(t *testing.T) {
	iss := relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	// 64 kbit/s is 8000 bytes/s with a 2000 byte burst.
	addr, srv := startLimitedRelay(t, relay.ServerConfig{AllocationMaxBPS: 64000, Issuer: iss})
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
	require.NoError(t, clientA.CreatePermission(relayB.LocalAddr()))
	require.NoError(t, clientB.CreatePermission(relayA.LocalAddr()))

	payload := make([]byte, 500)
	for range 40 {
		_, err = relayA.WriteTo(payload, relayB.LocalAddr())
		require.NoError(t, err)
	}

	assert.Eventually(t, func() bool { return srv.BandwidthDropped() > 0 }, 3*time.Second, 50*time.Millisecond)
}
