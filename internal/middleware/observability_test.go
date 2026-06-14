package middleware_test

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/middleware"
)

// fullyCapableWriter wraps httptest.ResponseRecorder with the optional
// interfaces a real http.Server exposes for HTTP/1.1 connections. We
// use it to assert the observability middleware doesn't strip them.
type fullyCapableWriter struct {
	*httptest.ResponseRecorder
	hijackCalled bool
	flushCalled  bool
}

var errStubHijack = errors.New("stub hijack")

func (f *fullyCapableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijackCalled = true
	return nil, nil, errStubHijack
}

func (f *fullyCapableWriter) Flush() {
	f.flushCalled = true
}

func (f *fullyCapableWriter) ReadFrom(_ interface{ Read([]byte) (int, error) }) (int64, error) {
	return 0, nil
}

// TestObservabilityMiddleware_preserves_optional_interfaces guards
// against the bug class that produced the /v1/ws 501 in production:
// a wrapper around http.ResponseWriter that drops the optional
// interfaces (Hijacker, Flusher, …) silently breaks WebSocket,
// SSE, and HTTP/2 push at runtime. The chi WrapResponseWriter helper
// we delegate to is supposed to surface every optional interface the
// underlying writer actually exposes. This test asserts that pinky-swear.
func TestObservabilityMiddleware_preserves_optional_interfaces(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := middleware.NewObservability(reg)

	var sawHijacker, sawFlusher bool
	var hijackErr error
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			sawHijacker = true
			_, _, hijackErr = hj.Hijack()
		}
		if fl, ok := w.(http.Flusher); ok {
			sawFlusher = true
			fl.Flush()
		}
	}))

	underlying := &fullyCapableWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	handler.ServeHTTP(underlying, req)

	require.True(t, sawHijacker, "wrapper must expose http.Hijacker when underlying writer does — /v1/ws upgrades break otherwise")
	require.True(t, sawFlusher, "wrapper must expose http.Flusher when underlying writer does — SSE breaks otherwise")
	assert.ErrorIs(t, hijackErr, errStubHijack, "wrapper must forward Hijack to the underlying writer, not synthesize its own implementation")
	assert.True(t, underlying.hijackCalled, "underlying Hijack should have been invoked")
	assert.True(t, underlying.flushCalled, "underlying Flush should have been invoked")
}
