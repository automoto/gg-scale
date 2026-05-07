// Package fleet implements an in-memory registry of game-server instances
// that have announced themselves to ggscale via /v1/fleet/servers.
//
// The registry is intentionally ephemeral: heartbeats drive lifetime, and the
// canonical state lives only in this process. That's appropriate for the
// Tier-0 self-hosting topology (one ggscale-server, single host). Phase 2's
// allocation engine replaces this with a Docker-aware scheduler.
package fleet

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a server is unknown or the caller does not
// own it (the registry treats both cases the same way to avoid leaking the
// existence of other tenants' servers).
var ErrNotFound = errors.New("fleet: server not found")

// ErrInvalid is returned when register parameters are missing required fields.
var ErrInvalid = errors.New("fleet: invalid parameters")

// Server is a single registered game-server instance.
type Server struct {
	ID            uuid.UUID
	TenantID      int64
	ProjectID     int64
	Name          string
	Address       string
	Version       string
	Region        string
	MaxPlayers    int
	LastHeartbeat time.Time
}

// RegisterParams is the input to Register.
type RegisterParams struct {
	TenantID   int64
	ProjectID  int64
	Name       string
	Address    string
	Version    string
	Region     string
	MaxPlayers int
}

// Registry holds the active set of game servers.
type Registry struct {
	mu      sync.RWMutex
	ttl     time.Duration
	now     func() time.Time
	servers map[uuid.UUID]*Server
}

// NewRegistry returns a registry that treats entries with no heartbeat
// within ttl as expired.
func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{
		ttl:     ttl,
		now:     time.Now,
		servers: make(map[uuid.UUID]*Server),
	}
}

// Register inserts a new server and returns its assigned id. Required
// fields: TenantID, ProjectID, Address.
func (r *Registry) Register(p RegisterParams) (uuid.UUID, error) {
	if p.TenantID == 0 || p.ProjectID == 0 || p.Address == "" {
		return uuid.Nil, ErrInvalid
	}
	id := uuid.New()
	s := &Server{
		ID:            id,
		TenantID:      p.TenantID,
		ProjectID:     p.ProjectID,
		Name:          p.Name,
		Address:       p.Address,
		Version:       p.Version,
		Region:        p.Region,
		MaxPlayers:    p.MaxPlayers,
		LastHeartbeat: r.now(),
	}
	r.mu.Lock()
	r.servers[id] = s
	r.mu.Unlock()
	return id, nil
}

// Heartbeat refreshes a server's LastHeartbeat. Returns ErrNotFound if id is
// unknown or owned by a different tenant.
func (r *Registry) Heartbeat(tenantID int64, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[id]
	if !ok || s.TenantID != tenantID {
		return ErrNotFound
	}
	s.LastHeartbeat = r.now()
	return nil
}

// Deregister removes a server. Returns ErrNotFound if id is unknown or
// owned by a different tenant.
func (r *Registry) Deregister(tenantID int64, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[id]
	if !ok || s.TenantID != tenantID {
		return ErrNotFound
	}
	delete(r.servers, id)
	return nil
}

// List returns active (non-expired) servers in the given project.
func (r *Registry) List(tenantID, projectID int64) []Server {
	cutoff := r.now().Add(-r.ttl)
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Server, 0)
	for _, s := range r.servers {
		if s.TenantID != tenantID || s.ProjectID != projectID {
			continue
		}
		if s.LastHeartbeat.Before(cutoff) {
			continue
		}
		out = append(out, *s)
	}
	return out
}

// Size returns the total number of map entries (including expired ones not
// yet swept). Mainly for tests and metrics.
func (r *Registry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.servers)
}

// Run sweeps expired entries on a ticker until ctx is canceled. Without it
// the map grows unbounded as ephemeral servers register and vanish without
// deregistering.
func (r *Registry) Run(ctx context.Context) {
	ticker := time.NewTicker(r.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep()
		}
	}
}

// Sweep removes expired entries from the map. List already filters by TTL,
// so calling Sweep is only required to reclaim memory.
func (r *Registry) Sweep() {
	cutoff := r.now().Add(-r.ttl)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.servers {
		if s.LastHeartbeat.Before(cutoff) {
			delete(r.servers, id)
		}
	}
}
