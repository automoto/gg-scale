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

// playerAllocationGenerator applies the drain gate, the player and tenant
// allocation caps, the player budget, and the bandwidth limit at Pion's relay
// allocation boundary. Pion supplies the stable authenticated
// UserID only after MESSAGE-INTEGRITY succeeds. Empty identities, including
// EVEN-PORT preflight calls, fail closed before reaching the node allocator.
// Only new Allocate requests pass through here, so a draining node keeps
// refreshing and relaying for the allocations it already holds.
type playerAllocationGenerator struct {
	inner     pionturn.RelayAddressGenerator
	drain     *lifecycle.Gate
	limiter   *playerAllocLimiter
	throttled *atomic.Int64

	// Optional; nil caps and maxBPS 0 turn them off.
	caps    *subjectAllocCaps
	maxBPS  int64
	dropped *atomic.Int64
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
	// Each admitted step adds its undo. They all run once, when the relayed
	// socket closes or when a later step refuses the allocation.
	var releases []func()
	releaseAll := func() {
		for _, r := range releases {
			r()
		}
	}
	// The drain gate goes first so a refused allocation spends no cap slot or
	// token. An admitted Allocate counts as one unit of drain work from here
	// until its relayed socket closes, so setup and the live allocation are
	// never counted twice or not at all.
	if g.drain != nil {
		if !g.drain.Enter() {
			return nil, nil, errRelayDraining
		}
		releases = append(releases, g.drain.Leave)
	}
	if g.caps != nil {
		tenantID, playerID, ok := parseAllocationUserID(conf.UserID)
		if !ok {
			releaseAll()
			return nil, nil, errAllocationSubjectUnavailable
		}
		if err := g.caps.acquire(tenantID, playerID); err != nil {
			releaseAll()
			return nil, nil, err
		}
		releases = append(releases, func() { g.caps.release(tenantID, playerID) })
	}
	if err := g.allow(conf.UserID); err != nil {
		releaseAll()
		return nil, nil, err
	}
	conn, addr, err := g.inner.AllocatePacketConn(conf)
	if err != nil {
		releaseAll()
		return nil, nil, err
	}
	conn = newBandwidthLimitedConn(conn, g.maxBPS, g.dropped)
	return &countedPacketConn{PacketConn: conn, release: releaseAll}, addr, nil
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
