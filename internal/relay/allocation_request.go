package relay

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/pion/stun/v2"
	pionturn "github.com/pion/turn/v3"
)

var (
	errAllocationSubjectUnavailable = errors.New("relay: authenticated allocation subject unavailable")
	errPlayerAllocationThrottled    = errors.New("relay: player allocation rate limit reached")
)

// allocationRequestTracker connects one Allocate request read from a TURN
// transport to the RelayAddressGenerator call Pion makes after authenticating
// that request. Pion processes one request at a time per PacketConn. TCP and TLS
// connections sharing a listener are serialized only while an Allocate request
// is being handled, so the listener's shared generator sees the correct
// subject.
type allocationRequestTracker struct {
	gate    sync.Mutex
	current allocationRequest
	active  bool
}

type allocationRequest struct {
	tenantID int64
	playerID int64
	decided  bool
	allowed  bool
}

type allocationRequestScope struct {
	tracker       *allocationRequestTracker
	transactionID [stun.TransactionIDSize]byte
}

// begin returns nil for packets that cannot create an allocation. A scope is
// held until Pion writes the matching response or asks for the next request.
func (t *allocationRequestTracker) begin(raw []byte, issuer *Issuer) *allocationRequestScope {
	tenantID, playerID, transactionID, ok := allocationSubject(raw, issuer)
	if !ok {
		return nil
	}

	t.gate.Lock()
	t.current = allocationRequest{tenantID: tenantID, playerID: playerID}
	t.active = true
	return &allocationRequestScope{tracker: t, transactionID: transactionID}
}

func (s *allocationRequestScope) matchesResponse(raw []byte) bool {
	if s == nil || !stun.IsMessage(raw) {
		return false
	}
	m := &stun.Message{Raw: raw}
	if err := m.Decode(); err != nil || m.Type.Method != stun.MethodAllocate {
		return false
	}
	if m.Type.Class != stun.ClassSuccessResponse && m.Type.Class != stun.ClassErrorResponse {
		return false
	}
	return m.TransactionID == s.transactionID
}

func (s *allocationRequestScope) end() {
	if s == nil || s.tracker == nil {
		return
	}
	t := s.tracker
	t.current = allocationRequest{}
	t.active = false
	s.tracker = nil
	t.gate.Unlock()
}

// allow makes at most one limiter decision per Allocate request. Pion may call
// its generator more than once while satisfying EVEN-PORT, but that is still
// one client operation and consumes only one token.
func (t *allocationRequestTracker) allow(limiter *playerAllocLimiter) (allowed, first bool, err error) {
	if !t.active {
		return false, false, errAllocationSubjectUnavailable
	}
	if t.current.decided {
		return t.current.allowed, false, nil
	}
	t.current.decided = true
	t.current.allowed = limiter.allow(t.current.tenantID, t.current.playerID)
	return t.current.allowed, true, nil
}

func allocationSubject(raw []byte, issuer *Issuer) (tenantID, playerID int64, transactionID [stun.TransactionIDSize]byte, ok bool) {
	if issuer == nil || !stun.IsMessage(raw) {
		return 0, 0, transactionID, false
	}
	m := &stun.Message{Raw: raw}
	if err := m.Decode(); err != nil || m.Type.Class != stun.ClassRequest || m.Type.Method != stun.MethodAllocate {
		return 0, 0, transactionID, false
	}
	var username stun.Username
	if err := username.GetFrom(m); err != nil {
		return 0, 0, transactionID, false
	}
	tenantID, playerID, _, err := issuer.parseUsername(username.String())
	return tenantID, playerID, m.TransactionID, err == nil
}

// allocationScopeSlot owns the current scope for one input connection. Pion
// can write relayed data concurrently with its request loop, so reads and
// response writes coordinate through this small lock rather than racing over a
// raw scope pointer.
type allocationScopeSlot struct {
	mu    sync.Mutex
	scope *allocationRequestScope
}

func (s *allocationScopeSlot) replace(scope *allocationRequestScope) {
	s.clear()
	s.mu.Lock()
	s.scope = scope
	s.mu.Unlock()
}

func (s *allocationScopeSlot) clear() {
	s.mu.Lock()
	scope := s.scope
	s.scope = nil
	s.mu.Unlock()
	scope.end()
}

// clearResponse releases the listener-wide gate before a matching response is
// written. A client that stops reading therefore cannot hold other
// connections' Allocate requests behind a blocked stream write.
func (s *allocationScopeSlot) clearResponse(raw []byte) {
	s.mu.Lock()
	scope := s.scope
	if scope == nil || !scope.matchesResponse(raw) {
		s.mu.Unlock()
		return
	}
	s.scope = nil
	s.mu.Unlock()
	scope.end()
}

// allocationTrackingPacketConn exposes each datagram to the request tracker
// before unmodified Pion reads it.
type allocationTrackingPacketConn struct {
	net.PacketConn
	tracker *allocationRequestTracker
	issuer  *Issuer
	scope   allocationScopeSlot
}

func (c *allocationTrackingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.scope.clear()

	n, addr, err := c.PacketConn.ReadFrom(p)
	if err == nil {
		c.scope.replace(c.tracker.begin(p[:n], c.issuer))
	}
	return n, addr, err
}

func (c *allocationTrackingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.scope.clearResponse(p)
	return c.PacketConn.WriteTo(p, addr)
}

// allocationTrackingListener frames accepted streams one TURN packet at a
// time before Pion frames them again. Returning exactly one frame per Read is
// what lets a listener-wide tracker release one request before admitting the
// next connection's Allocate request.
type allocationTrackingListener struct {
	net.Listener
	tracker *allocationRequestTracker
	issuer  *Issuer
}

func (l *allocationTrackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &allocationTrackingConn{
		Conn:    conn,
		framed:  pionturn.NewSTUNConn(conn),
		tracker: l.tracker,
		issuer:  l.issuer,
	}, nil
}

type allocationTrackingConn struct {
	net.Conn
	framed  net.PacketConn
	tracker *allocationRequestTracker
	issuer  *Issuer
	scope   allocationScopeSlot
}

func (c *allocationTrackingConn) Read(p []byte) (int, error) {
	c.scope.clear()

	n, _, err := c.framed.ReadFrom(p)
	if err == nil {
		c.scope.replace(c.tracker.begin(p[:n], c.issuer))
	}
	return n, err
}

func (c *allocationTrackingConn) Write(p []byte) (int, error) {
	c.scope.clearResponse(p)
	return c.Conn.Write(p)
}

// playerAllocationGenerator applies the player budget at Pion's public relay
// allocation boundary. Reaching this method proves Pion accepted
// MESSAGE-INTEGRITY for the tracked request.
type playerAllocationGenerator struct {
	inner     pionturn.RelayAddressGenerator
	tracker   *allocationRequestTracker
	limiter   *playerAllocLimiter
	throttled *atomic.Int64
}

func (g *playerAllocationGenerator) Validate() error { return g.inner.Validate() }

func (g *playerAllocationGenerator) AllocatePacketConn(network string, requestedPort int) (net.PacketConn, net.Addr, error) {
	if err := g.allow(); err != nil {
		return nil, nil, err
	}
	return g.inner.AllocatePacketConn(network, requestedPort)
}

func (g *playerAllocationGenerator) AllocateConn(network string, requestedPort int) (net.Conn, net.Addr, error) {
	if err := g.allow(); err != nil {
		return nil, nil, err
	}
	return g.inner.AllocateConn(network, requestedPort)
}

func (g *playerAllocationGenerator) allow() error {
	allowed, first, err := g.tracker.allow(g.limiter)
	if err != nil {
		return err
	}
	if allowed {
		return nil
	}
	if first {
		g.throttled.Add(1)
	}
	return errPlayerAllocationThrottled
}

func trackPacketConn(conn net.PacketConn, issuer *Issuer, limiter *playerAllocLimiter, throttled *atomic.Int64, generator pionturn.RelayAddressGenerator) (net.PacketConn, pionturn.RelayAddressGenerator) {
	if limiter == nil {
		return conn, generator
	}
	tracker := &allocationRequestTracker{}
	return &allocationTrackingPacketConn{PacketConn: conn, tracker: tracker, issuer: issuer}, &playerAllocationGenerator{
		inner: generator, tracker: tracker, limiter: limiter, throttled: throttled,
	}
}

func trackListener(listener net.Listener, issuer *Issuer, limiter *playerAllocLimiter, throttled *atomic.Int64, generator pionturn.RelayAddressGenerator) (net.Listener, pionturn.RelayAddressGenerator) {
	if limiter == nil {
		return listener, generator
	}
	tracker := &allocationRequestTracker{}
	return &allocationTrackingListener{Listener: listener, tracker: tracker, issuer: issuer}, &playerAllocationGenerator{
		inner: generator, tracker: tracker, limiter: limiter, throttled: throttled,
	}
}
