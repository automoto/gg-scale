package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// ConnectionLimitStore resolves an optional persisted realtime admission
// envelope for one tenant. No row means the tenant's compiled tier envelope.
type ConnectionLimitStore interface {
	ConnectionLimit(ctx context.Context, tenantID int64) (CapLimits, bool, error)
}

// ConnectionLimitInvalidator drops a cached tenant connection envelope after
// an administrative write. The raw database store has nothing to invalidate.
type ConnectionLimitInvalidator interface {
	Invalidate(tenantID int64)
}

// DBConnectionLimitStore reads platform-managed connection overrides through
// BootstrapQ because the table is intentionally outside tenant RLS.
type DBConnectionLimitStore struct {
	pool *db.Pool
}

// NewDBConnectionLimitStore builds a Postgres-backed connection-limit store.
func NewDBConnectionLimitStore(pool *db.Pool) *DBConnectionLimitStore {
	return &DBConnectionLimitStore{pool: pool}
}

// ConnectionLimit implements ConnectionLimitStore.
func (s *DBConnectionLimitStore) ConnectionLimit(ctx context.Context, tenantID int64) (CapLimits, bool, error) {
	var out CapLimits
	found := false
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetConnectionLimitOverride(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = CapLimits{Sustained: row.Sustained, Ceiling: row.Ceiling}
		found = true
		return nil
	})
	if err != nil {
		return CapLimits{}, false, fmt.Errorf("ratelimit: connection override: %w", err)
	}
	return out, found, nil
}

type cachedConnectionLimit struct {
	limits    CapLimits
	found     bool
	expiresAt time.Time
}

// CachedConnectionLimitStore bounds database reads on the WebSocket handshake
// path. Administrative writes invalidate the local process immediately; other
// processes converge within the cache TTL.
type CachedConnectionLimitStore struct {
	inner ConnectionLimitStore
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[int64]cachedConnectionLimit
}

var _ ConnectionLimitInvalidator = (*CachedConnectionLimitStore)(nil)

// NewCachedConnectionLimitStore wraps inner with a per-tenant TTL cache.
func NewCachedConnectionLimitStore(inner ConnectionLimitStore, ttl time.Duration) *CachedConnectionLimitStore {
	if ttl <= 0 {
		ttl = DefaultOverrideCacheTTL
	}
	return &CachedConnectionLimitStore{
		inner: inner,
		ttl:   ttl,
		now:   time.Now,
		cache: make(map[int64]cachedConnectionLimit),
	}
}

// ConnectionLimit implements ConnectionLimitStore.
func (c *CachedConnectionLimitStore) ConnectionLimit(ctx context.Context, tenantID int64) (CapLimits, bool, error) {
	now := c.now()
	c.mu.Lock()
	if entry, ok := c.cache[tenantID]; ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.limits, entry.found, nil
	}
	c.mu.Unlock()

	limits, found, err := c.inner.ConnectionLimit(ctx, tenantID)
	if err != nil {
		return CapLimits{}, false, err
	}
	c.mu.Lock()
	c.cache[tenantID] = cachedConnectionLimit{limits: limits, found: found, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return limits, found, nil
}

// Invalidate evicts one tenant so an administrative change takes effect on the
// writer process immediately.
func (c *CachedConnectionLimitStore) Invalidate(tenantID int64) {
	c.mu.Lock()
	delete(c.cache, tenantID)
	c.mu.Unlock()
}
