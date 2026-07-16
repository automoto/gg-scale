package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/middleware"
)

func TestRequestDeadline_returns_503_when_handler_exceeds_deadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	h := middleware.NewRequestDeadline(30 * time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release // never returns before the deadline fires
			w.WriteHeader(http.StatusOK)
		}))

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Less(t, elapsed, time.Second, "middleware must respond at the deadline, not wait for the handler")
	assert.Contains(t, rec.Body.String(), "request_timeout")
}

func TestRequestDeadline_passes_fast_handler_through_unchanged(t *testing.T) {
	h := middleware.NewRequestDeadline(time.Second)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "yes")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("hello"))
		}))

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "yes", rec.Header().Get("X-Custom"))
	assert.Equal(t, "hello", rec.Body.String())
}

func TestRequestDeadline_exempts_websocket_upgrade(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reached := make(chan struct{})

	var hadDeadline bool
	h := middleware.NewRequestDeadline(20 * time.Millisecond)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, hadDeadline = r.Context().Deadline()
			close(reached)
			<-release
		}))

	req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()

	go h.ServeHTTP(rec, req)

	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("handler was not reached for a websocket upgrade")
	}

	assert.False(t, hadDeadline, "websocket upgrades must not get a request deadline")
}
