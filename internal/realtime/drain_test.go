package realtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/realtime"
)

// newDrainServer serves ServeWS behind drain.Guard, the way the router mounts
// /v1/ws.
func newDrainServer(t *testing.T, hub *realtime.Hub, drain *realtime.Drain, playerID int64) string {
	t.Helper()
	opts := realtime.Options{Hub: hub, Drain: drain, HeartbeatInterval: time.Hour}
	srv := httptest.NewServer(drain.Guard(wrap(1, playerID, realtime.ServeWS(opts))))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialWS(t *testing.T, url string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if conn != nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, resp, err
}

func waitForCount(t *testing.T, drain *realtime.Drain, want int64) {
	t.Helper()
	require.Eventually(t, func() bool { return drain.Count() == want }, 2*time.Second, 5*time.Millisecond)
}

func TestDrainGuard_should_refuse_upgrade_with_503_while_draining(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	drain.BeginDrain()

	_, resp, err := dialWS(t, url)

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestDrainGuard_should_count_refused_upgrades(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	drain.BeginDrain()

	_, _, _ = dialWS(t, url)

	assert.Equal(t, int64(1), drain.RefusedWork())
}

func TestDrainGuard_should_accept_upgrades_after_drain_ends(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	drain.BeginDrain()
	drain.EndDrain()

	_, _, err := dialWS(t, url)

	assert.NoError(t, err)
}

func TestDrain_should_count_open_sockets(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)

	_, _, err := dialWS(t, url)
	require.NoError(t, err)

	waitForCount(t, drain, 1)
}

func TestDrain_should_release_count_when_socket_closes(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	conn, _, err := dialWS(t, url)
	require.NoError(t, err)
	waitForCount(t, drain, 1)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))

	waitForCount(t, drain, 0)
}

func TestDrain_should_keep_delivering_to_existing_socket_while_draining(t *testing.T) {
	hub := realtime.NewHub()
	drain := realtime.NewDrain()
	url := newDrainServer(t, hub, drain, 42)
	conn, _, err := dialWS(t, url)
	require.NoError(t, err)
	// The drain counts the socket from admission, before the hub registers
	// it, so wait for the hub before sending.
	require.Eventually(t, func() bool { return hub.Count() == 1 }, 2*time.Second, 5*time.Millisecond)
	drain.BeginDrain()

	require.NoError(t, hub.Send(context.Background(), 1, 42, realtime.Message{Type: "ping"}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"ping"}`, string(data))
}

func TestDrainCloseAll_should_close_sockets_with_going_away(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	conn, _, err := dialWS(t, url)
	require.NoError(t, err)
	waitForCount(t, drain, 1)
	// A client reads to receive the close frame and answer it.
	readErr := make(chan error, 1)
	go func() {
		_, _, rerr := conn.Read(context.Background())
		readErr <- rerr
	}()

	drain.CloseAll(context.Background())

	select {
	case err := <-readErr:
		assert.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
	case <-time.After(5 * time.Second):
		t.Fatal("socket was not closed")
	}
}

// readUntilClosed reads conn in the background, the way a client does to
// answer a close frame, and reports the read error.
func readUntilClosed(conn *websocket.Conn) <-chan error {
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := conn.Read(context.Background()); err != nil {
				readErr <- err
				return
			}
		}
	}()
	return readErr
}

func TestDrain_should_count_admitted_upgrade_before_socket_opens(t *testing.T) {
	drain := realtime.NewDrain()
	var during int64
	h := drain.Guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		during = drain.Count()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/ws", nil))

	assert.Equal(t, int64(1), during)
}

func TestDrainCloseAll_should_return_after_socket_handlers_finish(t *testing.T) {
	drain := realtime.NewDrain()
	var cleanedUp atomic.Bool
	ws := realtime.ServeWS(realtime.Options{Hub: realtime.NewHub(), Drain: drain, HeartbeatInterval: time.Hour})
	// Stands in for ServeWS's deferred slot and tenant cap releases.
	slowCleanup := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.ServeHTTP(w, r)
		time.Sleep(100 * time.Millisecond)
		cleanedUp.Store(true)
	})
	srv := httptest.NewServer(drain.Guard(wrap(1, 42, slowCleanup)))
	t.Cleanup(srv.Close)
	conn, _, err := dialWS(t, "ws"+strings.TrimPrefix(srv.URL, "http"))
	require.NoError(t, err)
	waitForCount(t, drain, 1)
	readUntilClosed(conn)

	drain.CloseAll(context.Background())

	assert.True(t, cleanedUp.Load())
}

func TestDrainCloseAll_should_close_socket_opened_after_it_ran(t *testing.T) {
	drain := realtime.NewDrain()
	url := newDrainServer(t, realtime.NewHub(), drain, 42)
	drain.CloseAll(context.Background())
	conn, _, err := dialWS(t, url)
	require.NoError(t, err)

	readErr := readUntilClosed(conn)

	select {
	case err := <-readErr:
		assert.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
	case <-time.After(5 * time.Second):
		t.Fatal("socket was not closed")
	}
}

func TestDrainCloseAll_should_return_when_no_sockets_are_open(t *testing.T) {
	drain := realtime.NewDrain()

	done := make(chan struct{})
	go func() {
		drain.CloseAll(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseAll blocked with no sockets")
	}
}
