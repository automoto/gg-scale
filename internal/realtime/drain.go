package realtime

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/automoto/gg-scale/internal/lifecycle"
)

// drainRetryAfter is advertised to a client refused by a draining node, so its
// SDK reconnects through the load balancer to another node.
const drainRetryAfter = 1 * time.Second

// drainPollInterval is how often CloseAll checks whether every socket handler
// has finished.
const drainPollInterval = 10 * time.Millisecond

// Drain tracks this process's open realtime sockets for the lifecycle
// contract. While draining, Guard refuses new upgrades with 503; sockets
// already open keep working until CloseAll. Safe for concurrent use.
type Drain struct {
	lifecycle.Gate

	mu      sync.Mutex
	conns   map[*wsWriter]struct{}
	closing bool
}

// NewDrain returns a Drain that admits upgrades.
func NewDrain() *Drain {
	return &Drain{conns: map[*wsWriter]struct{}{}}
}

// Guard refuses new upgrades with 503 while draining. It runs before
// authentication so a refused client spends no one-time ticket.
func (d *Drain) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.Enter() {
			w.Header().Set("Retry-After", strconv.Itoa(int(drainRetryAfter.Seconds())))
			http.Error(w, "server draining", http.StatusServiceUnavailable)
			return
		}
		defer d.Leave()
		next.ServeHTTP(w, r)
	})
}

// Count reports the number of sockets that are open or still being set up. An
// admitted upgrade counts until its handler returns, so a socket is counted
// while it authenticates and while its deferred cleanup (slot and tenant cap
// release, hub unregister) runs.
func (d *Drain) Count() int64 {
	return d.Entered()
}

// track registers an open socket for CloseAll. A socket that opens after
// CloseAll has run is closed with 1001 Going Away at once.
func (d *Drain) track(w *wsWriter) func() {
	d.mu.Lock()
	d.conns[w] = struct{}{}
	closing := d.closing
	d.mu.Unlock()
	if closing {
		go func() { _ = w.closeWith(websocket.StatusGoingAway, "server shutting down") }()
	}
	return func() {
		d.mu.Lock()
		delete(d.conns, w)
		d.mu.Unlock()
	}
}

// CloseAll closes every open socket with 1001 Going Away, in parallel, and
// returns once every socket handler has finished its cleanup or ctx is done.
func (d *Drain) CloseAll(ctx context.Context) {
	d.mu.Lock()
	d.closing = true
	writers := make([]*wsWriter, 0, len(d.conns))
	for w := range d.conns {
		writers = append(writers, w)
	}
	d.mu.Unlock()

	for _, w := range writers {
		go func() { _ = w.closeWith(websocket.StatusGoingAway, "server shutting down") }()
	}
	ticker := time.NewTicker(drainPollInterval)
	defer ticker.Stop()
	for d.Count() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
