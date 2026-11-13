package relay

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queuedPacketConn returns its queued packets from ReadFrom, then
// net.ErrClosed, and records every WriteTo.
type queuedPacketConn struct {
	net.PacketConn
	queue   [][]byte
	written int
}

func (c *queuedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(c.queue) == 0 {
		return 0, nil, net.ErrClosed
	}
	n := copy(p, c.queue[0])
	c.queue = c.queue[1:]
	return n, &net.UDPAddr{}, nil
}

func (c *queuedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.written++
	return len(p), nil
}

// frozenBandwidthConn wraps inner at 2 Mbit/s with a stopped clock, so only
// the 250 ms burst (62500 bytes) is available.
func frozenBandwidthConn(inner net.PacketConn, dropped *atomic.Int64) *bandwidthLimitedConn {
	conn := newBandwidthLimitedConn(inner, 2000000, dropped).(*bandwidthLimitedConn)
	now := time.Unix(1000, 0)
	conn.now = func() time.Time { return now }
	return conn
}

func packets(count, size int) [][]byte {
	out := make([][]byte, count)
	for i := range out {
		out[i] = make([]byte, size)
	}
	return out
}

func readAll(conn net.PacketConn) int {
	buf := make([]byte, 1600)
	got := 0
	for {
		if _, _, err := conn.ReadFrom(buf); err != nil {
			return got
		}
		got++
	}
}

func TestBandwidthLimitedConn_should_drop_inbound_packets_over_burst(t *testing.T) {
	var dropped atomic.Int64
	conn := frozenBandwidthConn(&queuedPacketConn{queue: packets(70, 1000)}, &dropped)

	got := readAll(conn)

	assert.Equal(t, 62, got)
}

func TestBandwidthLimitedConn_should_count_dropped_inbound_packets(t *testing.T) {
	var dropped atomic.Int64
	conn := frozenBandwidthConn(&queuedPacketConn{queue: packets(70, 1000)}, &dropped)

	readAll(conn)

	assert.Equal(t, int64(8), dropped.Load())
}

func TestBandwidthLimitedConn_should_drop_outbound_packets_over_burst(t *testing.T) {
	var dropped atomic.Int64
	inner := &queuedPacketConn{}
	conn := frozenBandwidthConn(inner, &dropped)

	for _, p := range packets(70, 1000) {
		n, err := conn.WriteTo(p, &net.UDPAddr{})
		require.NoError(t, err)
		require.Equal(t, len(p), n, "a dropped write still reports success, it is not queued")
	}

	assert.Equal(t, [2]int64{62, 8}, [2]int64{int64(inner.written), dropped.Load()})
}

func TestBandwidthLimitedConn_should_refill_over_time(t *testing.T) {
	var dropped atomic.Int64
	inner := &queuedPacketConn{}
	conn := frozenBandwidthConn(inner, &dropped)
	for _, p := range packets(62, 1000) {
		_, _ = conn.WriteTo(p, &net.UDPAddr{})
	}
	later := time.Unix(1001, 0)
	conn.now = func() time.Time { return later }

	_, _ = conn.WriteTo(make([]byte, 1000), &net.UDPAddr{})

	assert.Equal(t, 63, inner.written)
}

func TestBandwidthLimitedConn_should_limit_directions_independently(t *testing.T) {
	var dropped atomic.Int64
	inner := &queuedPacketConn{queue: packets(1, 1000)}
	conn := frozenBandwidthConn(inner, &dropped)
	for _, p := range packets(62, 1000) {
		_, _ = conn.WriteTo(p, &net.UDPAddr{})
	}

	assert.Equal(t, 1, readAll(conn), "outbound traffic did not spend the inbound budget")
}

func TestNewBandwidthLimitedConn_should_return_conn_unchanged_when_disabled(t *testing.T) {
	inner := &queuedPacketConn{}

	conn := newBandwidthLimitedConn(inner, 0, &atomic.Int64{})

	assert.Same(t, inner, conn)
}

func TestNewBandwidthLimitedConn_should_admit_a_full_packet_at_low_rates(t *testing.T) {
	var dropped atomic.Int64
	conn := newBandwidthLimitedConn(&queuedPacketConn{}, 8000, &dropped)

	_, _ = conn.WriteTo(make([]byte, 1600), &net.UDPAddr{})

	assert.Zero(t, dropped.Load())
}
