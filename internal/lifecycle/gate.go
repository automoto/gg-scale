package lifecycle

import (
	"net/http"
	"strings"
	"sync/atomic"
)

// Gate is the accept/refuse switch a Drainable uses for new work. The zero
// value admits work. Safe for concurrent use.
type Gate struct {
	draining atomic.Bool
	refused  atomic.Int64
	entered  atomic.Int64
}

// BeginDrain makes Admit refuse new work.
func (g *Gate) BeginDrain() { g.draining.Store(true) }

// EndDrain makes Admit accept new work again.
func (g *Gate) EndDrain() { g.draining.Store(false) }

// Draining reports whether new work is being refused.
func (g *Gate) Draining() bool { return g.draining.Load() }

// Admit reports whether one unit of new work may start. A refusal is counted.
func (g *Gate) Admit() bool {
	if !g.draining.Load() {
		return true
	}
	g.refused.Add(1)
	return false
}

// Enter admits one unit of new work, like Admit, and counts it as in progress
// until Leave. The count is raised before the drain flag is read, and the
// Controller sets the flag before it reads the work count. Go's atomics are
// sequentially consistent, so a drain either refuses the work here or sees it
// in Entered. It never misses admitted work.
func (g *Gate) Enter() bool {
	g.entered.Add(1)
	if g.Admit() {
		return true
	}
	g.entered.Add(-1)
	return false
}

// Leave ends one unit of work admitted by Enter.
func (g *Gate) Leave() { g.entered.Add(-1) }

// Entered reports the work admitted by Enter that has not left yet.
func (g *Gate) Entered() int64 { return g.entered.Load() }

// RefusedWork reports the cumulative count of refused work.
func (g *Gate) RefusedWork() int64 { return g.refused.Load() }

// Inflight counts plain HTTP requests in progress. WebSocket upgrades to
// UpgradePath are not counted: a socket outlives its upgrade request, and the
// process that owns sockets counts them itself. An upgrade header on any other
// route is counted like a plain request, so a client cannot hide a long
// request from the drain. The lifecycle routes are not counted either, so a
// deploy tool polling GET /lifecycle can see active_work reach 0.
type Inflight struct {
	// UpgradePath is the one route whose WebSocket upgrades are not counted.
	// Empty counts every upgrade.
	UpgradePath string

	n atomic.Int64
}

// Count reports the number of requests in progress.
func (i *Inflight) Count() int64 { return i.n.Load() }

// Middleware counts each request that passes through next.
func (i *Inflight) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i.isSocketUpgrade(r) || isLifecycleRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		i.n.Add(1)
		defer i.n.Add(-1)
		next.ServeHTTP(w, r)
	})
}

func (i *Inflight) isSocketUpgrade(r *http.Request) bool {
	return i.UpgradePath != "" && r.URL.Path == i.UpgradePath &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
