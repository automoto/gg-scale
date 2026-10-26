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

// Drain tracks this process's open realtime sockets for the lifecycle
// contract. While draining, Guard refuses new upgrades with 503; sockets
// already open keep working until CloseAll. Safe for concurrent use.
type Drain struct {
	lifecycle.Gate

	mu    sync.Mutex
	conns map[*wsWriter]struct{}
}

// NewDrain returns a Drain that admits upgrades.
func NewDrain() *Drain {
	return &Drain{conns: map[*wsWriter]struct{}{}}
}

// Guard refuses new upgrades with 503 while draining. It runs before
// authentication so a refused client spends no one-time ticket.
func (d *Drain) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.Admit() {
			w.Header().Set("Retry-After", strconv.Itoa(int(drainRetryAfter.Seconds())))
			http.Error(w, "server draining", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Count reports the number of open sockets.
func (d *Drain) Count() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return int64(len(d.conns))
}

func (d *Drain) track(w *wsWriter) func() {
	d.mu.Lock()
	d.conns[w] = struct{}{}
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.conns, w)
		d.mu.Unlock()
	}
}

// CloseAll closes every open socket with 1001 Going Away, in parallel, and
// returns when all close handshakes finish or ctx is done.
func (d *Drain) CloseAll(ctx context.Context) {
	d.mu.Lock()
	writers := make([]*wsWriter, 0, len(d.conns))
	for w := range d.conns {
		writers = append(writers, w)
	}
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Go(func() { _ = w.closeWith(websocket.StatusGoingAway, "server shutting down") })
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
