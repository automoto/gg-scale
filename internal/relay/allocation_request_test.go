package relay

import (
	"net"
	"sync/atomic"
	"testing"

	pionturn "github.com/pion/turn/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/lifecycle"
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
	generator := newPlayerAllocationGenerator(inner, nil, newPlayerAllocLimiter(6, 1), &throttled)
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
	generator := newPlayerAllocationGenerator(inner, nil, newPlayerAllocLimiter(6, 1), &throttled)

	_, _, err := generator.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4"})

	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
	assert.Empty(t, inner.packetConfigs, "an uncorrelated allocation never reaches the node allocator")
	assert.Zero(t, throttled.Load())
}

func TestPlayerAllocationGeneratorRejectsMalformedUserID(t *testing.T) {
	inner := &recordingRelayGenerator{}
	generator := newPlayerAllocationGenerator(inner, nil, newPlayerAllocLimiter(6, 1), nil)

	_, _, err := generator.AllocatePacketConn(pionturn.AllocateListenerConfig{
		Network: "udp4",
		UserID:  "not-a-stable-subject",
	})

	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
	assert.Empty(t, inner.packetConfigs)
}

func TestPlayerAllocationGeneratorRejectsRFC6062(t *testing.T) {
	inner := &recordingRelayGenerator{}
	generator := newPlayerAllocationGenerator(inner, nil, newPlayerAllocLimiter(6, 1), nil)

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

func TestPlayerAllocationGenerator_should_refuse_without_spending_player_token_when_draining(t *testing.T) {
	inner := &recordingRelayGenerator{}
	var drain lifecycle.Gate
	generator := newPlayerAllocationGenerator(inner, &drain, newPlayerAllocLimiter(6, 1), nil)
	conf := pionturn.AllocateListenerConfig{Network: "udp4", UserID: allocationUserID(1, 42)}
	drain.BeginDrain()
	_, _, refusedErr := generator.AllocatePacketConn(conf)
	drain.EndDrain()

	_, _, err := generator.AllocatePacketConn(conf)

	assert.ErrorIs(t, refusedErr, errRelayDraining)
	assert.NoError(t, err, "the single-token budget is still available after the refusal")
}

// blockingRelayGenerator holds AllocatePacketConn open until release closes,
// so a test can observe an Allocate that is still being set up.
type blockingRelayGenerator struct {
	recordingRelayGenerator
	entered chan struct{}
	release chan struct{}
}

func (g *blockingRelayGenerator) AllocatePacketConn(pionturn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	close(g.entered)
	<-g.release
	return nil, &net.UDPAddr{}, nil
}

func TestPlayerAllocationGenerator_should_count_allocate_in_progress_as_drain_work(t *testing.T) {
	inner := &blockingRelayGenerator{entered: make(chan struct{}), release: make(chan struct{})}
	var drain lifecycle.Gate
	generator := newPlayerAllocationGenerator(inner, &drain, nil, nil)
	done := make(chan struct{})
	go func() {
		_, _, _ = generator.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4", UserID: allocationUserID(1, 42)})
		close(done)
	}()
	<-inner.entered
	t.Cleanup(func() {
		close(inner.release)
		<-done
	})

	drain.BeginDrain()

	assert.Equal(t, int64(1), drain.Entered())
}

func TestPlayerAllocationGenerator_should_release_drain_work_when_allocate_returns(t *testing.T) {
	inner := &recordingRelayGenerator{}
	var drain lifecycle.Gate
	generator := newPlayerAllocationGenerator(inner, &drain, nil, nil)

	_, _, err := generator.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4", UserID: allocationUserID(1, 42)})

	require.NoError(t, err)
	assert.Equal(t, int64(0), drain.Entered())
}

func TestPlayerAllocationGenerator_should_not_count_missing_subject_as_refused_work(t *testing.T) {
	var drain lifecycle.Gate
	generator := newPlayerAllocationGenerator(&recordingRelayGenerator{}, &drain, nil, nil)
	drain.BeginDrain()

	_, _, _ = generator.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4"})

	assert.Equal(t, int64(0), drain.RefusedWork())
}
