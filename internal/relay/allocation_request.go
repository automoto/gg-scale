package relay

import (
	"errors"
	"net"
	"sync/atomic"

	pionturn "github.com/pion/turn/v5"

	"github.com/automoto/gg-scale/internal/lifecycle"
)

var (
	errAllocationSubjectUnavailable = errors.New("relay: authenticated allocation subject unavailable")
	errPlayerAllocationThrottled    = errors.New("relay: player allocation rate limit reached")
	errRelayDraining                = errors.New("relay: node is draining; new allocations are refused")
)

// playerAllocationGenerator applies the drain gate and the player budget at
// Pion's relay allocation boundary. Pion supplies the stable authenticated
// UserID only after MESSAGE-INTEGRITY succeeds. Empty identities, including
// EVEN-PORT preflight calls, fail closed before reaching the node allocator.
// Only new Allocate requests pass through here, so a draining node keeps
// refreshing and relaying for the allocations it already holds.
type playerAllocationGenerator struct {
	inner     pionturn.RelayAddressGenerator
	drain     *lifecycle.Gate
	limiter   *playerAllocLimiter
	throttled *atomic.Int64
}

func newPlayerAllocationGenerator(
	inner pionturn.RelayAddressGenerator,
	drain *lifecycle.Gate,
	limiter *playerAllocLimiter,
	throttled *atomic.Int64,
) *playerAllocationGenerator {
	return &playerAllocationGenerator{inner: inner, drain: drain, limiter: limiter, throttled: throttled}
}

func (g *playerAllocationGenerator) Validate() error { return g.inner.Validate() }

func (g *playerAllocationGenerator) AllocatePacketConn(
	conf pionturn.AllocateListenerConfig,
) (net.PacketConn, net.Addr, error) {
	if conf.UserID == "" {
		return nil, nil, errAllocationSubjectUnavailable
	}
	if g.drain == nil {
		if err := g.allow(conf.UserID); err != nil {
			return nil, nil, err
		}
		return g.inner.AllocatePacketConn(conf)
	}
	// Checked before the player budget so a refused allocation spends no
	// token. An admitted Allocate counts as one unit of drain work from here
	// until its relayed socket closes, so setup and the live allocation are
	// never counted twice or not at all.
	if !g.drain.Enter() {
		return nil, nil, errRelayDraining
	}
	if err := g.allow(conf.UserID); err != nil {
		g.drain.Leave()
		return nil, nil, err
	}
	conn, addr, err := g.inner.AllocatePacketConn(conf)
	if err != nil {
		g.drain.Leave()
		return nil, nil, err
	}
	return &countedPacketConn{PacketConn: conn, release: g.drain.Leave}, addr, nil
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
