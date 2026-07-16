package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NewRequestDeadline bounds how long a non-streaming request may run. It runs
// the handler against a buffered writer under a context deadline; if the
// handler exceeds the deadline the request returns 503 + Retry-After instead
// of queuing on a saturated connection pool until the server WriteTimeout
// force-closes the connection. Because pgx honors the request context, an
// Acquire on an exhausted pool unblocks as soon as the deadline fires.
//
// WebSocket upgrades and other hijacked/streaming paths are exempt: they are
// long-lived by design and cannot be response-buffered. They pass through with
// the original writer and no added deadline.
func NewRequestDeadline(timeout time.Duration) func(http.Handler) http.Handler {
	retrySec := int(math.Ceil(timeout.Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)

			tw := &timeoutWriter{header: make(http.Header)}
			done := make(chan struct{})
			panicChan := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicChan <- p
					}
				}()
				next.ServeHTTP(tw, r)
				close(done)
			}()

			select {
			case p := <-panicChan:
				panic(p)
			case <-done:
				tw.flushTo(w)
			case <-ctx.Done():
				tw.markTimedOut()
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":               "request_timeout",
					"retry_after_seconds": retrySec,
				})
			}
		})
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// timeoutWriter buffers the handler's response so the deadline path and the
// handler goroutine can never both write to the real ResponseWriter. Once
// timed out, all handler writes are silently dropped.
type timeoutWriter struct {
	header http.Header

	mu          sync.Mutex
	buf         bytes.Buffer
	code        int
	wroteHeader bool
	timedOut    bool
}

func (tw *timeoutWriter) Header() http.Header { return tw.header }

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut || tw.wroteHeader {
		return
	}
	tw.code = code
	tw.wroteHeader = true
}

func (tw *timeoutWriter) Write(b []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if !tw.wroteHeader {
		tw.code = http.StatusOK
		tw.wroteHeader = true
	}
	return tw.buf.Write(b)
}

func (tw *timeoutWriter) markTimedOut() {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	tw.timedOut = true
}

// flushTo copies the buffered response onto the real writer. Called only on
// the completion path, after the handler goroutine has returned.
func (tw *timeoutWriter) flushTo(w http.ResponseWriter) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	dst := w.Header()
	for k, vv := range tw.header {
		dst[k] = vv
	}
	code := tw.code
	if !tw.wroteHeader {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(tw.buf.Bytes())
}
