package realtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRealtimeConnection struct {
	frames     chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls atomic.Int64
	pingErr    error
}

func newFakeRealtimeConnection(frameCount int) *fakeRealtimeConnection {
	frames := make(chan struct{}, frameCount)
	for range frameCount {
		frames <- struct{}{}
	}
	if frameCount > 0 {
		close(frames)
	}
	return &fakeRealtimeConnection{
		frames: frames,
		closed: make(chan struct{}),
	}
}

func (c *fakeRealtimeConnection) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-c.closed:
		return 0, nil, io.EOF
	case _, ok := <-c.frames:
		if !ok {
			return 0, nil, io.EOF
		}
		return websocket.MessageText, nil, nil
	}
}

func (c *fakeRealtimeConnection) Ping(context.Context) error {
	return c.pingErr
}

func (c *fakeRealtimeConnection) CloseNow() error {
	c.closeCalls.Add(1)
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

type fakeConnectionClock struct {
	now         time.Time
	ticks       chan time.Time
	tickerReady chan struct{}
	readyOnce   sync.Once
}

func newFakeConnectionClock() *fakeConnectionClock {
	return &fakeConnectionClock{
		now:         time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC),
		ticks:       make(chan time.Time, 1),
		tickerReady: make(chan struct{}),
	}
}

func (c *fakeConnectionClock) Now() time.Time {
	return c.now
}

func (c *fakeConnectionClock) NewTicker(time.Duration) connectionTicker {
	c.readyOnce.Do(func() { close(c.tickerReady) })
	return fakeConnectionTicker{ticks: c.ticks}
}

func (c *fakeConnectionClock) Tick() {
	c.ticks <- c.now
}

type fakeConnectionTicker struct {
	ticks <-chan time.Time
}

func (t fakeConnectionTicker) C() <-chan time.Time {
	return t.ticks
}

func (fakeConnectionTicker) Stop() {}

func TestRunConnection_heartbeatPingFailureClosesConnectionAndReturns(t *testing.T) {
	conn := newFakeRealtimeConnection(0)
	conn.pingErr = errors.New("ping failed")
	clock := newFakeConnectionClock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})

	go func() {
		runConnectionWithClock(ctx, conn, time.Minute, nil, slog.Default(), clock)
		close(done)
	}()
	select {
	case <-clock.tickerReady:
	case <-time.After(time.Second):
		t.Fatal("heartbeat ticker was not created")
	}
	clock.Tick()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runConnection did not return after heartbeat failure")
	}
	assert.Equal(t, int64(1), conn.closeCalls.Load())
}

type connectionContextKey struct{}

func TestRunConnection_rapidInboundFramesRefreshSlotsOncePerHeartbeatInterval(t *testing.T) {
	conn := newFakeRealtimeConnection(20)
	clock := newFakeConnectionClock()
	ctx := context.WithValue(t.Context(), connectionContextKey{}, "connection")
	var refreshCalls atomic.Int64
	var hasDeadline, hasParentValue atomic.Bool
	refreshSlots := func(refreshCtx context.Context) {
		refreshCalls.Add(1)
		_, deadlineSet := refreshCtx.Deadline()
		hasDeadline.Store(deadlineSet)
		hasParentValue.Store(refreshCtx.Value(connectionContextKey{}) == "connection")
	}

	runConnectionWithClock(ctx, conn, time.Minute, refreshSlots, slog.Default(), clock)

	require.Equal(t, int64(1), refreshCalls.Load())
	assert.True(t, hasDeadline.Load())
	assert.True(t, hasParentValue.Load())
}
