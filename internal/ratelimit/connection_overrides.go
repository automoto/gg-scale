package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// ConnectionLimitStore resolves an optional persisted realtime admission
// envelope for one tenant. No row means the tenant's compiled tier envelope.
// A caching implementation may return its last known value together with an
// error when a refresh fails; callers should record the error and use the value.
type ConnectionLimitStore interface {
	ConnectionLimit(ctx context.Context, tenantID int64) (CapLimits, bool, error)
}

// ConnectionLimitInvalidator drops a cached tenant connection envelope after
// an administrative write. The raw database store has nothing to invalidate.
type ConnectionLimitInvalidator interface {
	Invalidate(tenantID int64)
}

// ConnectionLimitOverrideStore is the managed-service store used by both the
// realtime read path and the control-panel invalidation path.
type ConnectionLimitOverrideStore interface {
	ConnectionLimitStore
	ConnectionLimitInvalidator
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

	mu         sync.Mutex
	cache      map[int64]cachedConnectionLimit
	generation map[int64]uint64
	loads      singleflight.Group
}

var _ ConnectionLimitOverrideStore = (*CachedConnectionLimitStore)(nil)

// NewCachedConnectionLimitStore wraps inner with a per-tenant TTL cache.
func NewCachedConnectionLimitStore(inner ConnectionLimitStore, ttl time.Duration) *CachedConnectionLimitStore {
	if ttl <= 0 {
		ttl = DefaultOverrideCacheTTL
	}
	return &CachedConnectionLimitStore{
		inner:      inner,
		ttl:        ttl,
		now:        time.Now,
		cache:      make(map[int64]cachedConnectionLimit),
		generation: make(map[int64]uint64),
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

	key := strconv.FormatInt(tenantID, 10)
	loaded, err, _ := c.loads.Do(key, func() (any, error) {
		return c.load(ctx, tenantID)
	})
	entry := loaded.(cachedConnectionLimit)
	return entry.limits, entry.found, err
}

func (c *CachedConnectionLimitStore) load(ctx context.Context, tenantID int64) (cachedConnectionLimit, error) {
	now := c.now()
	c.mu.Lock()
	entry, hadEntry := c.cache[tenantID]
	if hadEntry && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry, nil
	}
	generation := c.generation[tenantID]
	c.mu.Unlock()

	limits, found, err := c.inner.ConnectionLimit(ctx, tenantID)
	now = c.now()
	if err == nil {
		entry = cachedConnectionLimit{limits: limits, found: found, expiresAt: now.Add(c.ttl)}
	} else {
		// Preserve the last known override through a short database failure. If
		// this tenant has never been cached, the zero entry means "use its tier
		// default." The short backoff prevents every handshake from retrying the
		// same failed query while still converging quickly after recovery.
		if !hadEntry {
			entry = cachedConnectionLimit{}
		}
		entry.expiresAt = now.Add(min(c.ttl, time.Second))
	}

	c.mu.Lock()
	if c.generation[tenantID] == generation {
		c.cache[tenantID] = entry
	}
	c.mu.Unlock()
	return entry, err
}

// Invalidate evicts one tenant so an administrative change takes effect on the
// writer process immediately.
func (c *CachedConnectionLimitStore) Invalidate(tenantID int64) {
	c.mu.Lock()
	c.generation[tenantID]++
	delete(c.cache, tenantID)
	c.mu.Unlock()
	c.loads.Forget(strconv.FormatInt(tenantID, 10))
}
