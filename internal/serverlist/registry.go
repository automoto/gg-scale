// Package serverlist holds the in-memory registry of game-server
// heartbeats that powers the server-browser endpoint. Game-server
// processes POST a Heartbeat every ~5s; clients read the aggregated
// list via GET /v1/fleets/{fleet}/servers.
//
// Entries are tenant-isolated and evicted after TTL. The registry is
// safe for concurrent use.
package serverlist

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Heartbeat is the payload a game-server POSTs to announce itself.
// AgonesName is the unique key (the Agones GameServer CR name) used to
// dedupe replays. TenantID is set from the auth context, not the body,
// so a tenant can't spoof another tenant's fleet.
type Heartbeat struct {
	AgonesName     string
	Fleet          string
	Address        string
	Region         string
	Name           string
	CurrentPlayers int
	MaxPlayers     int
	GameMode       string
	Level          string
	Version        string
	TenantID       int64
}

// Server is the public projection returned by List. Mirrors the JSON
// the SDK and clients consume.
type Server struct {
	Name           string `json:"name"`
	Address        string `json:"address"`
	Region         string `json:"region"`
	CurrentPlayers int    `json:"current_players"`
	MaxPlayers     int    `json:"max_players"`
	GameMode       string `json:"game_mode"`
	Level          string `json:"level"`
	Version        string `json:"version"`
}

type entry struct {
	hb       Heartbeat
	lastSeen time.Time
}

const defaultMaxEntriesPerTenant = 1000

// ErrTenantLimitExceeded is returned when a new heartbeat would exceed a tenant's live entry cap.
var ErrTenantLimitExceeded = errors.New("serverlist: tenant entry limit exceeded")

// Registry tracks live game-servers keyed by (tenant, fleet, agones_name).
type Registry struct {
	ttl                 time.Duration
	maxEntriesPerTenant int
	now                 func() time.Time
	mu                  sync.RWMutex
	items               map[string]entry
}

// New returns a Registry with the given TTL. Heartbeats that haven't
// been refreshed within ttl are considered stale and dropped on List/Sweep.
func New(ttl time.Duration) *Registry {
	return &Registry{
		ttl:                 ttl,
		maxEntriesPerTenant: defaultMaxEntriesPerTenant,
		now:                 time.Now,
		items:               make(map[string]entry),
	}
}

// NewWithLimit is like New but caps each tenant's live server-list entries.
func NewWithLimit(ttl time.Duration, maxEntriesPerTenant int) *Registry {
	r := New(ttl)
	r.maxEntriesPerTenant = maxEntriesPerTenant
	return r
}

// NewWithClock is like New but uses the given clock — for deterministic tests.
func NewWithClock(ttl time.Duration, now func() time.Time) *Registry {
	r := New(ttl)
	r.now = now
	return r
}

func key(tenantID int64, fleet, agonesName string) string {
	return strconv.FormatInt(tenantID, 10) + ":" + fleet + ":" + agonesName
}

// Submit upserts a heartbeat. The TenantID on hb is the authority; pass
// it from the request's authenticated tenant context, not from the body.
func (r *Registry) Submit(hb Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key(hb.TenantID, hb.Fleet, hb.AgonesName)
	if _, ok := r.items[k]; !ok && r.tenantEntryCountLocked(hb.TenantID) >= r.maxEntriesPerTenant {
		return ErrTenantLimitExceeded
	}
	r.items[k] = entry{
		hb:       hb,
		lastSeen: r.now(),
	}
	return nil
}

// List returns the live servers for (tenantID, fleet), sorted by Name +
// Address (stable for clients rendering a list). Stale entries are
// filtered out lazily on read.
func (r *Registry) List(tenantID int64, fleet string) []Server {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cutoff := r.now().Add(-r.ttl)
	out := make([]Server, 0, 8)
	for _, e := range r.items {
		if e.hb.TenantID != tenantID || e.hb.Fleet != fleet {
			continue
		}
		if e.lastSeen.Before(cutoff) {
			continue
		}
		out = append(out, Server{
			Name:           e.hb.Name,
			Address:        e.hb.Address,
			Region:         e.hb.Region,
			CurrentPlayers: e.hb.CurrentPlayers,
			MaxPlayers:     e.hb.MaxPlayers,
			GameMode:       e.hb.GameMode,
			Level:          e.hb.Level,
			Version:        e.hb.Version,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// Sweep removes expired entries. Called by RunGC on a ticker; safe to
// call manually in tests.
func (r *Registry) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-r.ttl)
	for k, e := range r.items {
		if e.lastSeen.Before(cutoff) {
			delete(r.items, k)
		}
	}
}

func (r *Registry) tenantEntryCountLocked(tenantID int64) int {
	cutoff := r.now().Add(-r.ttl)
	count := 0
	for k, e := range r.items {
		if e.lastSeen.Before(cutoff) {
			delete(r.items, k)
			continue
		}
		if e.hb.TenantID == tenantID {
			count++
		}
	}
	return count
}

// RunGC sweeps expired entries every interval until ctx is cancelled.
// Mount as a goroutine in cmd/ggscale-server.
func (r *Registry) RunGC(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep()
		}
	}
}
