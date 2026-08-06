package relay

import (
	"net"
	"sync/atomic"
	"testing"

	pionturn "github.com/pion/turn/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRelayGenerator struct {
	packetConfigs   []pionturn.AllocateListenerConfig
	listenerConfigs []pionturn.AllocateListenerConfig
	connConfigs     []pionturn.AllocateConnConfig
}

func (*recordingRelayGenerator) Validate() error { return nil }

func (g *recordingRelayGenerator) AllocatePacketConn(conf pionturn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	g.packetConfigs = append(g.packetConfigs, conf)
	return nil, &net.UDPAddr{}, nil
}

func (g *recordingRelayGenerator) AllocateListener(conf pionturn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	g.listenerConfigs = append(g.listenerConfigs, conf)
	return nil, &net.TCPAddr{}, nil
}

func (g *recordingRelayGenerator) AllocateConn(conf pionturn.AllocateConnConfig) (net.Conn, error) {
	g.connConfigs = append(g.connConfigs, conf)
	return nil, nil
}

func TestPlayerAllocationGeneratorThrottlesStableAuthenticatedSubject(t *testing.T) {
	inner := &recordingRelayGenerator{}
	var throttled atomic.Int64
	generator := newPlayerAllocationGenerator(inner, newPlayerAllocLimiter(6, 1), &throttled)
	conf := pionturn.AllocateListenerConfig{Network: "udp4", UserID: allocationUserID(1, 42)}

	_, _, err := generator.AllocatePacketConn(conf)
	require.NoError(t, err)
	_, _, err = generator.AllocatePacketConn(conf)

	assert.ErrorIs(t, err, errPlayerAllocationThrottled)
	assert.Len(t, inner.packetConfigs, 1, "a throttled allocation never reaches the node allocator")
	assert.Equal(t, int64(1), throttled.Load())
}

func TestPlayerAllocationGeneratorRejectsMissingAuthenticatedSubject(t *testing.T) {
	inner := &recordingRelayGenerator{}
	var throttled atomic.Int64
	generator := newPlayerAllocationGenerator(inner, newPlayerAllocLimiter(6, 1), &throttled)

	_, _, err := generator.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4"})

	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
	assert.Empty(t, inner.packetConfigs, "an uncorrelated allocation never reaches the node allocator")
	assert.Zero(t, throttled.Load())
}

func TestPlayerAllocationGeneratorRejectsMalformedUserID(t *testing.T) {
	inner := &recordingRelayGenerator{}
	generator := newPlayerAllocationGenerator(inner, newPlayerAllocLimiter(6, 1), nil)

	_, _, err := generator.AllocatePacketConn(pionturn.AllocateListenerConfig{
		Network: "udp4",
		UserID:  "not-a-stable-subject",
	})

	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
	assert.Empty(t, inner.packetConfigs)
}

func TestPlayerAllocationGeneratorRejectsRFC6062(t *testing.T) {
	inner := &recordingRelayGenerator{}
	generator := newPlayerAllocationGenerator(inner, newPlayerAllocLimiter(6, 1), nil)

	_, _, listenerErr := generator.AllocateListener(pionturn.AllocateListenerConfig{
		Network: "tcp4",
		UserID:  allocationUserID(1, 42),
	})
	_, connErr := generator.AllocateConn(pionturn.AllocateConnConfig{
		Network: "tcp4",
		UserID:  allocationUserID(1, 42),
	})

	assert.ErrorIs(t, listenerErr, errTCPRelayUnsupported)
	assert.ErrorIs(t, connErr, errTCPRelayUnsupported)
	assert.Empty(t, inner.listenerConfigs)
	assert.Empty(t, inner.connConfigs)
}
