package realtime_test

import (
	"context"
	"encoding/json"
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
	require.Len(t, w.Writes(), 1)
	assert.JSONEq(t, `{"type":"presence","payload":{"status":"online"}}`, string(w.Writes()[0]))
}

func TestHubPush_should_not_cross_tenants(t *testing.T) {
	a, b, _ := twoHosts(t)
	w := &fakeWriter{}
	defer b.Register(2, 42, w)()

	_ = a.Push(context.Background(), 1, 42, realtime.Message{Type: "presence"})

	assert.Empty(t, w.Writes())
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
