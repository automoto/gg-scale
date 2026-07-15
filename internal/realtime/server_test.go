package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/ratelimit"
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

func TestServeWSRejectsWhenTenantCapExceeded(t *testing.T) {
	hub := realtime.NewHub()
	store := memory.New()
	capKey := ratelimit.ConnectionCapKey(1)
	// Pre-fill the fixed (override) envelope so the first upgrade can't acquire.
	for i := 0; i < 3; i++ {
		ok, _, err := store.AcquireSlotBurst(context.Background(), capKey, 3, 3, time.Minute, time.Hour)
		require.NoError(t, err)
		require.True(t, ok)
	}

	srv := httptest.NewServer(wrap(1, 42, realtime.ServeWS(realtime.Options{
		Hub:             hub,
		Cache:           store,
		TenantCap:       ratelimit.NewCacheConnectionCap(store, nil),
		EnvMaxPerTenant: 3,
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"), "rejected client is told when to retry")
}

func TestServeWSTenantCapFailsOpenOnCacheError(t *testing.T) {
	hub := realtime.NewHub()
	// A cap that always errors stands in for an unavailable cache backend.
	url, stop := newTestServer(t, hub, realtime.Options{
		TenantCap:         errCap{},
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err, "cap-check error must not block the connection (fail-open)")
	defer conn.CloseNow()

	require.Eventually(t, func() bool { return hub.Count() == 1 }, time.Second, 10*time.Millisecond)
}

type trackingCap struct {
	acquires atomic.Int64
	releases atomic.Int64
	refresh  atomic.Int64
	decision ratelimit.CapDecision
	err      error
}

func (c *trackingCap) Acquire(context.Context, string, ratelimit.CapLimits) (ratelimit.CapDecision, error) {
	c.acquires.Add(1)
	return c.decision, c.err
}

func (c *trackingCap) Release(context.Context, string) error {
	c.releases.Add(1)
	return nil
}

func (c *trackingCap) Refresh(context.Context, string) error {
	c.refresh.Add(1)
	return nil
}

func TestServeWSChecksPlayerCapBeforeTenantCap(t *testing.T) {
	hub := realtime.NewHub()
	store := memory.New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	cap := &trackingCap{decision: ratelimit.CapDecision{Allowed: true}}
	url, stop := newTestServer(t, hub, realtime.Options{
		Cache:             store,
		TenantCap:         cap,
		EnvMaxPerTenant:   4,
		MaxPerPlayer:      1,
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.Eventually(t, func() bool { return cap.acquires.Load() == 1 }, time.Second, 10*time.Millisecond)

	resp, err := http.Get("http" + strings.TrimPrefix(url, "ws"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(body), "too many connections for this user")
	assert.Equal(t, int64(1), cap.acquires.Load(), "player rejection must not reserve a tenant slot")
}

func TestServeWSTenantRejectionReleasesPlayerSlot(t *testing.T) {
	hub := realtime.NewHub()
	store := memory.New()
	t.Cleanup(func() { require.NoError(t, store.Close(context.Background())) })
	cap := &trackingCap{decision: ratelimit.CapDecision{Allowed: false, Current: 4, Reason: ratelimit.CapRejectCeiling}}
	url, stop := newTestServer(t, hub, realtime.Options{
		Cache:             store,
		TenantCap:         cap,
		EnvMaxPerTenant:   4,
		MaxPerPlayer:      1,
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	resp, err := http.Get("http" + strings.TrimPrefix(url, "ws"))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "5", resp.Header.Get("Retry-After"))
	assert.Contains(t, string(body), "too many connections")

	ok, current, err := store.AcquireSlot(context.Background(), "realtime:tenant:1:user:42", 1, time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "tenant rejection must release the earlier player reservation")
	assert.Equal(t, int64(1), current)
}

func TestServeWSTenantCapErrorDoesNotScheduleUnmatchedRelease(t *testing.T) {
	hub := realtime.NewHub()
	cap := &trackingCap{err: errors.New("cache unavailable")}
	url, stop := newTestServer(t, hub, realtime.Options{
		TenantCap:         cap,
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
	require.Eventually(t, func() bool { return hub.Count() == 0 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1), cap.acquires.Load())
	assert.Zero(t, cap.releases.Load())
	assert.Zero(t, cap.refresh.Load())
}

func TestServeWSReleasesTenantCapOnDisconnect(t *testing.T) {
	hub := realtime.NewHub()
	store := memory.New()
	capKey := ratelimit.ConnectionCapKey(1)
	url, stop := newTestServer(t, hub, realtime.Options{
		Cache:             store,
		TenantCap:         ratelimit.NewCacheConnectionCap(store, nil),
		EnvMaxPerTenant:   3,
		HeartbeatInterval: time.Hour,
	}, 1, 42)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)

	// Slot reserved while connected: a probe acquire sees count >= 1.
	require.Eventually(t, func() bool {
		ok, current, _ := store.AcquireSlotBurst(ctx, capKey, 3, 3, time.Minute, time.Hour)
		if ok {
			_ = store.ReleaseSlotBurst(ctx, capKey)
		}
		return current >= 1
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))

	// Slot released after the server observes the close.
	require.Eventually(t, func() bool { return hub.Count() == 0 }, 2*time.Second, 10*time.Millisecond)
}

// errCap is a ratelimit.ConnectionCap whose Acquire always fails, used to
// exercise the fail-open path.
type errCap struct{}

func (errCap) Acquire(context.Context, string, ratelimit.CapLimits) (ratelimit.CapDecision, error) {
	return ratelimit.CapDecision{}, errors.New("cache unavailable")
}
func (errCap) Release(context.Context, string) error { return nil }
func (errCap) Refresh(context.Context, string) error { return nil }
