package relay

import (
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// maxRelayPacket is pion's relay read buffer size. The burst never drops below
// it, so one full packet always fits.
const maxRelayPacket = 1600

// bandwidthLimitedConn caps one allocation's relayed socket at maxBPS bits per
// second in each direction: ReadFrom is peer -> relay, WriteTo is relay ->
// peer. Each direction is a token bucket with about 250 ms of burst. Packets
// over the limit are dropped, never queued.
type bandwidthLimitedConn struct {
	net.PacketConn
	in, out *rate.Limiter
	dropped *atomic.Int64
	now     func() time.Time
}

// newBandwidthLimitedConn returns conn unchanged when maxBPS <= 0.
func newBandwidthLimitedConn(conn net.PacketConn, maxBPS int64, dropped *atomic.Int64) net.PacketConn {
	if maxBPS <= 0 {
		return conn
	}
	bytesPerSecond := rate.Limit(float64(maxBPS) / 8)
	burst := max(int(maxBPS/32), maxRelayPacket) // 250 ms of traffic, in bytes
	return &bandwidthLimitedConn{
		PacketConn: conn,
		in:         rate.NewLimiter(bytesPerSecond, burst),
		out:        rate.NewLimiter(bytesPerSecond, burst),
		dropped:    dropped,
		now:        time.Now,
	}
}

func (c *bandwidthLimitedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || c.in.AllowN(c.now(), n) {
			return n, addr, err
		}
		c.dropped.Add(1)
	}
}

// WriteTo reports a dropped packet as sent: UDP gives no delivery promise.
func (c *bandwidthLimitedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.out.AllowN(c.now(), len(p)) {
		c.dropped.Add(1)
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}
