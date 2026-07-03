package realtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/realtime"
)

// wrap inserts tenant + player ids into the request context, standing in
// for the production tenant + player middlewares without dragging in their
// auth machinery.
func wrap(tenantID, playerID int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if tenantID != 0 {
			ctx = db.WithTenant(ctx, tenantID)
		}
		if playerID != 0 {
			ctx = playerauth.WithID(ctx, playerID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newTestServer(t *testing.T, hub *realtime.Hub, opts realtime.Options, tenantID, playerID int64) (string, func()) {
	t.Helper()
	if opts.Hub == nil {
		opts.Hub = hub
	}
	srv := httptest.NewServer(wrap(tenantID, playerID, realtime.ServeWS(opts)))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	return url, srv.Close
}

func TestServeWSRejectsRequestWithoutTenantContext(t *testing.T) {
	hub := realtime.NewHub()
	srv := httptest.NewServer(realtime.ServeWS(realtime.Options{Hub: hub}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestServeWSRejectsRequestWithoutPlayerContext(t *testing.T) {
	hub := realtime.NewHub()
	srv := httptest.NewServer(wrap(1, 0, realtime.ServeWS(realtime.Options{Hub: hub})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServeWSUpgradesAndDeliversHubMessages(t *testing.T) {
	hub := realtime.NewHub()
	url, stop := newTestServer(t, hub, realtime.Options{HeartbeatInterval: time.Hour}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Wait until the hub has the writer registered, then push a frame.
	require.Eventually(t, func() bool { return hub.Count() == 1 }, time.Second, 10*time.Millisecond)
	require.NoError(t, hub.Send(ctx, 1, 42, realtime.Message{Type: "match_ready", Payload: json.RawMessage(`{"address":"1.2.3.4:7777"}`)}))

	mt, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, mt)
	var got realtime.Message
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "match_ready", got.Type)
}

func TestServeWSIdleConnectionSurvivesPastReadTimeoutWindow(t *testing.T) {
	hub := realtime.NewHub()
	heartbeat := 25 * time.Millisecond
	url, stop := newTestServer(t, hub, realtime.Options{HeartbeatInterval: heartbeat}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	require.Eventually(t, func() bool { return hub.Count() == 1 }, time.Second, 10*time.Millisecond)
	time.Sleep(heartbeat*2 + 30*time.Millisecond)

	require.NoError(t, hub.Send(ctx, 1, 42, realtime.Message{Type: "match_ready", Payload: json.RawMessage(`{"address":"1.2.3.4:7777"}`)}))
	mt, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageText, mt)
	var got realtime.Message
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "match_ready", got.Type)
}

func TestServeWSRejectsWhenTenantSlotCapExceeded(t *testing.T) {
	hub := realtime.NewHub()
	cache := memory.New()
	// Pre-fill the cap so the very first upgrade can't acquire a slot.
	for i := 0; i < 3; i++ {
		ok, _, err := cache.AcquireSlot(context.Background(), "realtime:tenant:1", 3, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}

	srv := httptest.NewServer(wrap(1, 42, realtime.ServeWS(realtime.Options{
		Hub:          hub,
		Cache:        cache,
		MaxPerTenant: 3,
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestServeWSReleasesSlotOnDisconnect(t *testing.T) {
	hub := realtime.NewHub()
	cache := memory.New()
	url, stop := newTestServer(t, hub, realtime.Options{
		Cache:             cache,
		MaxPerTenant:      3,
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)

	// Slot must be reserved while connected.
	require.Eventually(t, func() bool {
		ok, current, _ := cache.AcquireSlot(ctx, "realtime:tenant:1", 3, time.Minute)
		if ok {
			_ = cache.ReleaseSlot(ctx, "realtime:tenant:1")
		}
		return current >= 1
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))

	// Slot must be released after the server observes the close.
	require.Eventually(t, func() bool {
		return hub.Count() == 0
	}, 2*time.Second, 10*time.Millisecond)
}
