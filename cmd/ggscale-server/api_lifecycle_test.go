package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/playerauth"
	"github.com/automoto/gg-scale/internal/realtime"
)

func newAPIDrainFixture(t *testing.T) (httpapi.APIWork, *lifecycle.Controller, string) {
	t.Helper()
	work := httpapi.NewAPIWork()
	lc := lifecycle.New(lifecycle.Config{Role: "api", Work: work, PollInterval: 5 * time.Millisecond})
	lc.MarkReady()
	ws := realtime.ServeWS(realtime.Options{Hub: realtime.NewHub(), Drain: work.Realtime, HeartbeatInterval: time.Hour})
	withIdentity := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := playerauth.WithID(db.WithTenant(r.Context(), 1), 42)
		ws.ServeHTTP(w, r.WithContext(ctx))
	})
	srv := httptest.NewServer(work.Inflight.Middleware(work.Realtime.Guard(withIdentity)))
	t.Cleanup(srv.Close)
	return work, lc, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestDrainRealtime_should_return_immediately_without_active_work(t *testing.T) {
	work, lc, _ := newAPIDrainFixture(t)

	start := time.Now()
	drainRealtime(lc, work, time.Minute)

	assert.Less(t, time.Since(start), time.Second)
}

func TestDrainRealtime_should_close_open_socket_with_going_away_after_timeout(t *testing.T) {
	work, lc, url := newAPIDrainFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.Eventually(t, func() bool { return work.Realtime.Count() == 1 }, 2*time.Second, 5*time.Millisecond)
	type closed struct {
		status websocket.StatusCode
		at     time.Time
	}
	got := make(chan closed, 1)
	go func() {
		_, _, rerr := conn.Read(ctx)
		got <- closed{websocket.CloseStatus(rerr), time.Now()}
	}()
	const drainTimeout = 200 * time.Millisecond

	start := time.Now()
	drainRealtime(lc, work, drainTimeout)

	c := <-got
	assert.Equal(t, websocket.StatusGoingAway, c.status)
	assert.Less(t, c.at.Sub(start), drainTimeout+500*time.Millisecond)
}

func TestDrainRealtime_should_refuse_new_upgrades(t *testing.T) {
	work, lc, url := newAPIDrainFixture(t)
	drainRealtime(lc, work, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, url, nil)

	require.Error(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
