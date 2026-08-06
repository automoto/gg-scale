package relay

import (
	"testing"

	pionturn "github.com/pion/turn/v5"
	"github.com/stretchr/testify/assert"
)

func TestAllocationLimiterRejectsTCPRelayListener(t *testing.T) {
	inner := &recordingRelayGenerator{}
	limiter := newAllocationLimiter(inner, 1)

	_, _, err := limiter.AllocateListener(pionturn.AllocateListenerConfig{
		Network: "tcp4",
		UserID:  allocationUserID(1, 42),
	})

	assert.ErrorIs(t, err, errTCPRelayUnsupported)
	assert.Zero(t, limiter.live.Load())
	assert.Zero(t, limiter.rejected.Load())
	assert.Empty(t, inner.listenerConfigs)
}

func TestAllocationLimiterRejectsOutboundTCPConnection(t *testing.T) {
	inner := &recordingRelayGenerator{}
	limiter := newAllocationLimiter(inner, 1)

	_, err := limiter.AllocateConn(pionturn.AllocateConnConfig{
		Network: "tcp4",
		UserID:  allocationUserID(1, 42),
	})

	assert.ErrorIs(t, err, errTCPRelayUnsupported)
	assert.Zero(t, limiter.live.Load())
	assert.Zero(t, limiter.rejected.Load())
	assert.Empty(t, inner.connConfigs)
}

func TestAllocationLimiterRejectsMissingSubjectBeforeCap(t *testing.T) {
	inner := &recordingRelayGenerator{}
	limiter := newAllocationLimiter(inner, 1)

	_, _, err := limiter.AllocatePacketConn(pionturn.AllocateListenerConfig{Network: "udp4"})

	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
	assert.Zero(t, limiter.live.Load())
	assert.Zero(t, limiter.rejected.Load())
	assert.Empty(t, inner.packetConfigs)
}
