package tenant

import (
	"context"
	"errors"
	"sync"
	"time"
)

// APIKeyCacheTTL bounds how long a cached key lookup is served. A revoked key,
// a disabled tenant, or a changed tier or scope takes effect within this time
// on every app instance; there is no cross-instance invalidation.
const APIKeyCacheTTL = 30 * time.Second

// APIKeyCacheMaxEntries bounds the cache size. When it is full, one entry is
// evicted at random.
const APIKeyCacheMaxEntries = 10000

type keyCacheEntry struct {
	key     *APIKey // nil means ErrUnknownKey
	expires time.Time
}

type keyCache struct {
	inner Lookup
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]keyCacheEntry
}

// NewCachedLookup wraps inner with an in-process cache of found and unknown
// keys, so repeated requests with the same key skip the database. Other
// lookup errors are not cached.
func NewCachedLookup(inner Lookup, ttl time.Duration, maxEntries int) Lookup {
	c := &keyCache{inner: inner, ttl: ttl, max: maxEntries, now: time.Now, entries: make(map[string]keyCacheEntry)}
	return c.lookup
}

func (c *keyCache) lookup(ctx context.Context, keyHash []byte) (*APIKey, error) {
	id := string(keyHash)
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[id]
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
	if _, exists := c.entries[id]; !exists && len(c.entries) >= c.max {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[id] = keyCacheEntry{key: key, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return copyKey(key)
}

// copyKey hands each caller its own APIKey: Admit writes to the one it gets.
func copyKey(k *APIKey) (*APIKey, error) {
	if k == nil {
		return nil, ErrUnknownKey
	}
	out := *k
	return &out, nil
}
