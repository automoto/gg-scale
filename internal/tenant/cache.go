package tenant

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/automoto/gg-scale/internal/cacheinvalidation"
)

// DefaultAPIKeyCacheTTL is the default of API_KEY_CACHE_TTL: how long a cached
// key lookup is served. A revoked key, a disabled tenant, or a changed tier or
// scope is evicted on the writing instance and on LISTEN/NOTIFY recipients; the
// TTL and 60-second fallback cover a lost notification.
const DefaultAPIKeyCacheTTL = 30 * time.Second

// APIKeyCacheMaxEntries bounds the cache size. When it is full, one entry is
// evicted at random.
const APIKeyCacheMaxEntries = 10000

type keyCacheEntry struct {
	key     *APIKey // nil means ErrUnknownKey
	expires time.Time
}

// KeyCache is an in-process cache of found and unknown keys in front of a
// Lookup, so repeated requests with the same key skip the database. Other
// lookup errors are not cached. A nil KeyCache caches nothing and its
// Invalidate does nothing.
type KeyCache struct {
	inner    Lookup
	ttl      time.Duration
	max      int
	now      func() time.Time
	notifier cacheinvalidation.Notifier

	mu      sync.Mutex
	entries map[string]keyCacheEntry
	// drops counts Invalidate calls, so a lookup that was in flight during
	// one does not store its result.
	drops uint64
}

// NewKeyCache wraps inner. A ttl of 0 or less uses DefaultAPIKeyCacheTTL.
func NewKeyCache(inner Lookup, ttl time.Duration, maxEntries int) *KeyCache {
	if ttl <= 0 {
		ttl = DefaultAPIKeyCacheTTL
	}
	return &KeyCache{inner: inner, ttl: ttl, max: maxEntries, now: time.Now, entries: make(map[string]keyCacheEntry)}
}

// WithNotifier configures cross-instance invalidation. Set it during startup,
// before serving requests.
func (c *KeyCache) WithNotifier(notifier cacheinvalidation.Notifier) *KeyCache {
	c.notifier = notifier
	return c
}

// Lookup implements the Lookup signature with caching.
func (c *KeyCache) Lookup(ctx context.Context, keyHash []byte) (*APIKey, error) {
	id := string(keyHash)
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[id]
	drops := c.drops
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return copyKey(e.key)
	}

	if !allowMiss(ctx) {
		return nil, errMissRefused
	}
	key, err := c.inner(ctx, keyHash)
	if err != nil && !errors.Is(err, ErrUnknownKey) {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drops != drops {
		// A write dropped entries while the database read ran. The row can
		// be the one from before the write, so it is not kept.
		return copyKey(key)
	}
	if _, exists := c.entries[id]; !exists && len(c.entries) >= c.max {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[id] = keyCacheEntry{key: key, expires: now.Add(c.ttl)}
	return copyKey(key)
}

// Invalidate drops the cached keys of a tenant on this process. A write path
// calls InvalidateAndNotify after its commit when it changes what a lookup
// returns: a key revoke or scope change, a tenant disable or enable, a tier
// change, or a connection limit change.
func (c *KeyCache) Invalidate(tenantID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drops++
	for id, e := range c.entries {
		if e.key != nil && e.key.TenantID == tenantID {
			delete(c.entries, id)
		}
	}
}

// InvalidateAndNotify evicts locally before publishing to other instances.
// Notification failure is non-fatal: the TTL and slow fallback remain active.
func (c *KeyCache) InvalidateAndNotify(ctx context.Context, tenantID int64) {
	if c == nil {
		return
	}
	c.Invalidate(tenantID)
	if c.notifier == nil {
		return
	}
	event := cacheinvalidation.Event{Kind: cacheinvalidation.KindAPIKey, TenantID: tenantID}
	if err := cacheinvalidation.Notify(ctx, c.notifier, event); err != nil {
		slog.WarnContext(ctx, "API key cache notification failed; TTL and fallback remain active", "error", err)
	}
}

// InvalidateAll drops every entry on this process for the fallback sweep.
func (c *KeyCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.entries)
	c.drops++
	c.mu.Unlock()
}

// copyKey hands each caller its own APIKey; callers must not mutate it.
func copyKey(k *APIKey) (*APIKey, error) {
	if k == nil {
		return nil, ErrUnknownKey
	}
	out := *k
	return &out, nil
}
