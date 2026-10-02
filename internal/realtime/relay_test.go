package realtime_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/realtime"
)

// fakeRelay connects hubs the way Postgres NOTIFY does: a notify reaches
// every listener, including the sender.
type fakeRelay struct {
	mu        sync.Mutex
	listeners []func(string)
	notified  []string
}

func (f *fakeRelay) Notify(_ context.Context, _, payload string) error {
	f.mu.Lock()
	f.notified = append(f.notified, payload)
	listeners := append([]func(string){}, f.listeners...)
	f.mu.Unlock()
	for _, fn := range listeners {
		fn(payload)
	}
	return nil
}

func (f *fakeRelay) ListenChannel(ctx context.Context, _ string, fn func(string)) error {
	f.mu.Lock()
	f.listeners = append(f.listeners, fn)
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (f *fakeRelay) waitListeners(n int) {
	for {
		f.mu.Lock()
		got := len(f.listeners)
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notified)
}

func twoHosts(t *testing.T) (*realtime.Hub, *realtime.Hub, *fakeRelay) {
	t.Helper()
	relay := &fakeRelay{}
	a, b := realtime.NewHub().WithRelay(relay), realtime.NewHub().WithRelay(relay)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.RunRelay(ctx)
	go b.RunRelay(ctx)
	relay.waitListeners(2)
	return a, b, relay
}

func TestHubPush_should_deliver_locally_without_relay(t *testing.T) {
	a, _, relay := twoHosts(t)
	w := &fakeWriter{}
	defer a.Register(1, 42, w)()

	err := a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence"})

	require.NoError(t, err)
	assert.Equal(t, 0, relay.count())
}

func TestHubPush_should_reach_player_on_other_host(t *testing.T) {
	a, b, _ := twoHosts(t)
	w := &fakeWriter{}
	defer b.Register(1, 42, w)()

	err := a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence", Payload: json.RawMessage(`{"status":"online"}`)})

	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(w.Writes()) == 1 }, time.Second, time.Millisecond)
	assert.JSONEq(t, `{"type":"presence","payload":{"status":"online"}}`, string(w.Writes()[0]))
}

func TestHubPush_should_not_cross_tenants(t *testing.T) {
	a, b, _ := twoHosts(t)
	w := &fakeWriter{}
	defer b.Register(2, 42, w)()

	_ = a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence"})

	assert.Never(t, func() bool { return len(w.Writes()) > 0 }, 50*time.Millisecond, time.Millisecond)
}

func TestHubPush_should_refuse_payload_over_notify_limit(t *testing.T) {
	a, _, relay := twoHosts(t)
	big := json.RawMessage(`"` + strings.Repeat("x", 8000) + `"`)

	err := a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence", Payload: big})

	assert.ErrorIs(t, err, realtime.ErrRelayTooLarge)
	assert.Equal(t, 0, relay.count())
}

func TestHubPush_should_report_not_connected_without_relay(t *testing.T) {
	h := realtime.NewHub()

	err := h.Push(context.Background(), 1, 42, realtime.Message{Type: "presence"})

	assert.ErrorIs(t, err, realtime.ErrNotConnected)
}

func TestHubPushMany_should_relay_remote_players_in_one_notify(t *testing.T) {
	a, b, relay := twoHosts(t)
	writers := []*fakeWriter{{}, {}, {}}
	for i, w := range writers {
		defer b.Register(1, int64(10+i), w)()
	}

	err := a.PushMany(context.Background(), 1, []int64{10, 11, 12}, realtime.Message{Type: "presence"})

	require.NoError(t, err)
	assert.Equal(t, 1, relay.count())
}

func TestHubPushMany_should_reach_each_remote_player(t *testing.T) {
	a, b, _ := twoHosts(t)
	writers := []*fakeWriter{{}, {}, {}}
	for i, w := range writers {
		defer b.Register(1, int64(10+i), w)()
	}

	_ = a.PushMany(context.Background(), 1, []int64{10, 11, 12}, realtime.Message{Type: "presence"})

	for _, w := range writers {
		assert.Eventually(t, func() bool { return len(w.Writes()) == 1 }, time.Second, time.Millisecond)
	}
}

func TestHubPushMany_should_split_large_player_lists(t *testing.T) {
	a, _, relay := twoHosts(t)
	players := make([]int64, 2000)
	for i := range players {
		players[i] = int64(1_000_000_000 + i)
	}

	err := a.PushMany(context.Background(), 1, players, realtime.Message{Type: "presence"})

	require.NoError(t, err)
	assert.Greater(t, relay.count(), 1)
}

// blockingWriter never finishes a write until released.
type blockingWriter struct{ release chan struct{} }

func (w *blockingWriter) Write(ctx context.Context, _ []byte) error {
	select {
	case <-w.release:
	case <-ctx.Done():
	}
	return nil
}

func (w *blockingWriter) Close() error { return nil }

func TestHubRelay_should_not_let_a_stuck_socket_block_other_players(t *testing.T) {
	a, b, _ := twoHosts(t)
	stuck := &blockingWriter{release: make(chan struct{})}
	defer close(stuck.release)
	defer b.Register(1, 41, stuck)()
	w := &fakeWriter{}
	defer b.Register(1, 42, w)()

	_ = a.Push(context.Background(), 1, 41, realtime.Message{Type: "presence"})
	_ = a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence"})

	assert.Eventually(t, func() bool { return len(w.Writes()) == 1 }, time.Second, time.Millisecond)
}

func TestHubRelay_should_keep_order_per_player(t *testing.T) {
	a, b, _ := twoHosts(t)
	w := &fakeWriter{}
	defer b.Register(1, 42, w)()
	const n = 50

	for i := range n {
		_ = a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence", Payload: json.RawMessage(strconv.Itoa(i))})
	}

	require.Eventually(t, func() bool { return len(w.Writes()) == n }, time.Second, time.Millisecond)
	for i, got := range w.Writes() {
		assert.JSONEq(t, `{"type":"presence","payload":`+strconv.Itoa(i)+`}`, string(got))
	}
}

func TestHubPushMany_should_send_once_per_player(t *testing.T) {
	h := realtime.NewHub()
	w := &fakeWriter{}
	defer h.Register(1, 42, w)()

	err := h.PushMany(context.Background(), 1, []int64{42, 42}, realtime.Message{Type: "presence"})

	require.NoError(t, err)
	assert.Len(t, w.Writes(), 1)
}
