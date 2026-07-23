package relay

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	pionturn "github.com/pion/turn/v3"
)

// allocationLimiter wraps a pion RelayAddressGenerator with a global cap on
// concurrently-live relay allocations plus active/rejected counters.
//
// One valid credential can otherwise call Allocate without limit until its TTL
// elapses, binding one relayed-media port each time and exhausting the node's
// port range (relay-node DoS). pion/turn v3 exposes no per-allocation lifecycle
// callback, so we account through the generator: every AllocatePacketConn /
// AllocateConn increments the live counter and the returned conn's Close
// decrements it. When the cap is reached, further allocations are refused with a
// counted error instead of degrading into pion's failed-bind retry loop.
//
// The cap is global, not per-credential: pion does not pass the username to the
// generator, so a true per-credential cap would need a pion fork (deferred, see
// docs/relay-ga.md M5). Per-player abuse is bounded upstream by the issuance
// rate limit and monthly meter; this cap is the relay-node backstop and the
// source of the active/rejected gauges.
type allocationLimiter struct {
	inner    pionturn.RelayAddressGenerator
	max      int64 // 0 = unlimited
	live     atomic.Int64
	rejected atomic.Int64
}

func newAllocationLimiter(inner pionturn.RelayAddressGenerator, max int) *allocationLimiter {
	return &allocationLimiter{inner: inner, max: int64(max)}
}

func (a *allocationLimiter) Validate() error { return a.inner.Validate() }

func (a *allocationLimiter) AllocatePacketConn(network string, requestedPort int) (net.PacketConn, net.Addr, error) {
	if !a.acquire() {
		return nil, nil, fmt.Errorf("relay: allocation cap %d reached", a.max)
	}
	conn, addr, err := a.inner.AllocatePacketConn(network, requestedPort)
	if err != nil {
		a.release()
		return nil, nil, err
	}
	return &countedPacketConn{PacketConn: conn, release: a.release}, addr, nil
}

func (a *allocationLimiter) AllocateConn(network string, requestedPort int) (net.Conn, net.Addr, error) {
	if !a.acquire() {
		return nil, nil, fmt.Errorf("relay: allocation cap %d reached", a.max)
	}
	conn, addr, err := a.inner.AllocateConn(network, requestedPort)
	if err != nil {
		a.release()
		return nil, nil, err
	}
	return &countedConn{Conn: conn, release: a.release}, addr, nil
}

// acquire reserves an allocation slot, returning false (and counting a
// rejection) when the cap is already reached. max <= 0 means unlimited.
func (a *allocationLimiter) acquire() bool {
	if n := a.live.Add(1); a.max > 0 && n > a.max {
		a.live.Add(-1)
		a.rejected.Add(1)
		return false
	}
	return true
}

func (a *allocationLimiter) release() { a.live.Add(-1) }

// countedPacketConn releases one allocation slot the first time it is closed.
type countedPacketConn struct {
	net.PacketConn
	release func()
	once    sync.Once
}

func (c *countedPacketConn) Close() error {
	c.once.Do(c.release)
	return c.PacketConn.Close()
}

// countedConn releases one allocation slot the first time it is closed.
type countedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
