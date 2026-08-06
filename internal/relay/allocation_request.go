package relay

import (
	"errors"
	"net"
	"sync/atomic"

	pionturn "github.com/pion/turn/v5"
)

var (
	errAllocationSubjectUnavailable = errors.New("relay: authenticated allocation subject unavailable")
	errPlayerAllocationThrottled    = errors.New("relay: player allocation rate limit reached")
)

// playerAllocationGenerator applies the player budget at Pion's relay
// allocation boundary. Pion supplies the stable authenticated UserID only
// after MESSAGE-INTEGRITY succeeds. Empty identities, including EVEN-PORT
// preflight calls, fail closed before reaching the node allocator.
type playerAllocationGenerator struct {
	inner     pionturn.RelayAddressGenerator
	limiter   *playerAllocLimiter
	throttled *atomic.Int64
}

func newPlayerAllocationGenerator(
	inner pionturn.RelayAddressGenerator,
	limiter *playerAllocLimiter,
	throttled *atomic.Int64,
) *playerAllocationGenerator {
	return &playerAllocationGenerator{inner: inner, limiter: limiter, throttled: throttled}
}

func (g *playerAllocationGenerator) Validate() error { return g.inner.Validate() }

func (g *playerAllocationGenerator) AllocatePacketConn(
	conf pionturn.AllocateListenerConfig,
) (net.PacketConn, net.Addr, error) {
	if err := g.allow(conf.UserID); err != nil {
		return nil, nil, err
	}
	return g.inner.AllocatePacketConn(conf)
}

func (g *playerAllocationGenerator) AllocateListener(
	pionturn.AllocateListenerConfig,
) (net.Listener, net.Addr, error) {
	return nil, nil, errTCPRelayUnsupported
}

func (g *playerAllocationGenerator) AllocateConn(pionturn.AllocateConnConfig) (net.Conn, error) {
	return nil, errTCPRelayUnsupported
}

func (g *playerAllocationGenerator) allow(userID string) error {
	if userID == "" {
		return errAllocationSubjectUnavailable
	}
	if g.limiter == nil {
		return nil
	}
	tenantID, playerID, ok := parseAllocationUserID(userID)
	if !ok {
		return errAllocationSubjectUnavailable
	}
	if g.limiter.allow(tenantID, playerID) {
		return nil
	}
	if g.throttled != nil {
		g.throttled.Add(1)
	}
	return errPlayerAllocationThrottled
}
