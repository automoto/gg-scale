package storagelimit

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LimitStore is the behavior consumers depend on; both *Store and *CachedStore
// implement it, so callers can be handed either a raw or a cached store.
type LimitStore interface {
	Resolve(ctx context.Context, tenantID, projectID, def int64) (int64, error)
	Set(ctx context.Context, updatedBy, tenantID int64, projectID *int64, maxBytes int64) error
	ListForTenant(ctx context.Context, tenantID int64) ([]Override, error)
}

var _ LimitStore = (*Store)(nil)
var _ LimitStore = (*CachedStore)(nil)

// DefaultCacheTTL matches the rate-limit override cache freshness.
const DefaultCacheTTL = 5 * time.Second

// CachedStore memoizes Resolve for a short TTL so the per-request storage write
// path doesn't hit Postgres on every PUT. Mirrors ratelimit.CachedOverrideStore.
//
// Writes go straight to the inner store and then drop the tenant's cached
// entries, so a change takes effect immediately on the process that served it;
// the TTL bounds staleness everywhere else (per-process cache, no cross-node
// invalidation, like the rate-limit cache).
type CachedStore struct {
	inner LimitStore
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]resolveEntry
}

type resolveEntry struct {
	value     int64
	expiresAt time.Time
}

// NewCachedStore wraps inner with a TTL cache.
func NewCachedStore(inner LimitStore, ttl time.Duration) *CachedStore {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &CachedStore{
		inner: inner,
		ttl:   ttl,
		now:   time.Now,
		cache: make(map[string]resolveEntry),
	}
}

// Resolve returns the cached effective limit, loading from the inner store on a
// miss. The default is part of the key so a caller passing a different fallback
// never reads another's cached value.
func (c *CachedStore) Resolve(ctx context.Context, tenantID, projectID, def int64) (int64, error) {
	key := strconv.FormatInt(tenantID, 10) + ":" + strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(def, 10)
	now := c.now()
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.value, nil
	}
	c.mu.Unlock()

	val, err := c.inner.Resolve(ctx, tenantID, projectID, def)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.cache[key] = resolveEntry{value: val, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return val, nil
}

// Set writes through to the inner store, then drops the tenant's cached entries
// so the next Resolve reflects the change. A tenant-row write shifts the ceiling
// for every project, so all of the tenant's entries are cleared.
func (c *CachedStore) Set(ctx context.Context, updatedBy, tenantID int64, projectID *int64, maxBytes int64) error {
	if err := c.inner.Set(ctx, updatedBy, tenantID, projectID, maxBytes); err != nil {
		return err
	}
	c.invalidate(tenantID)
	return nil
}

// ListForTenant delegates to the inner store (admin read, not on a hot path).
func (c *CachedStore) ListForTenant(ctx context.Context, tenantID int64) ([]Override, error) {
	return c.inner.ListForTenant(ctx, tenantID)
}

func (c *CachedStore) invalidate(tenantID int64) {
	prefix := strconv.FormatInt(tenantID, 10) + ":"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.cache {
		if strings.HasPrefix(k, prefix) {
			delete(c.cache, k)
		}
	}
}
