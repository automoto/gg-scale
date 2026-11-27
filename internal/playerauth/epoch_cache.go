package playerauth

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultSessionEpochCacheTTL is the default SESSION_EPOCH_CACHE_TTL.
const DefaultSessionEpochCacheTTL = 30 * time.Second

const sessionEpochCacheMaxEntries = 10000

// TokenEpochValidator can use the token's snapshot to avoid accepting a newly
// issued token against an older cached epoch. CurrentEpoch alone cannot express
// that case, so Admit prefers this interface when a validator implements it.
type TokenEpochValidator interface {
	CurrentEpochForToken(ctx context.Context, playerID, tokenEpoch int64) (int64, error)
}

type epochCacheEntry struct {
	epoch     int64
	expiresAt time.Time
}

// CachedEpochValidator caches successful epoch reads on one process. Caching is
// enabled only while SetHealthy(true) reports a live LISTEN subscription. Every
// disconnect or reconnect flushes the cache; while disconnected all reads go to
// Postgres so a missed notification cannot extend a revoked session's life.
type CachedEpochValidator struct {
	inner EpochValidator
	ttl   time.Duration
	now   func() time.Time

	mu         sync.Mutex
	healthy    bool
	entries    map[int64]epochCacheEntry
	generation uint64
	requests   *prometheus.CounterVec
}

// NewCachedEpochValidator wraps inner. ttl=0 disables caching. The cache starts
// unhealthy, and therefore uncached, until the invalidation listener is ready.
func NewCachedEpochValidator(inner EpochValidator, ttl time.Duration, reg prometheus.Registerer) *CachedEpochValidator {
	c := &CachedEpochValidator{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[int64]epochCacheEntry),
	}
	if reg != nil {
		requests := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_session_epoch_cache_requests_total",
			Help: "Player session-epoch checks served from cache, reloaded, or bypassed.",
		}, []string{"result"})
		if err := reg.Register(requests); err != nil {
			if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
				requests = already.ExistingCollector.(*prometheus.CounterVec)
			} else {
				panic(err)
			}
		}
		c.requests = requests
	}
	return c
}

// SetHealthy changes whether cache reads are safe. Both transitions flush all
// entries: a disconnect discards entries that could miss notifications, and a
// reconnect discards anything written before the listener was ready again.
func (c *CachedEpochValidator) SetHealthy(healthy bool) {
	c.mu.Lock()
	c.healthy = healthy
	c.generation++
	clear(c.entries)
	c.mu.Unlock()
}

// Invalidate drops one player after an epoch bump or player deletion.
func (c *CachedEpochValidator) Invalidate(playerID int64) {
	c.mu.Lock()
	delete(c.entries, playerID)
	c.generation++
	c.mu.Unlock()
}

// InvalidateAll drops every player, used when the notification stream resets.
func (c *CachedEpochValidator) InvalidateAll() {
	c.mu.Lock()
	clear(c.entries)
	c.generation++
	c.mu.Unlock()
}

// CurrentEpoch implements EpochValidator.
func (c *CachedEpochValidator) CurrentEpoch(ctx context.Context, playerID int64) (int64, error) {
	return c.currentEpoch(ctx, playerID, nil)
}

// CurrentEpochForToken implements TokenEpochValidator. A token epoch above the
// cached value forces a reload, so a newly refreshed token is never rejected
// merely because an older token populated the cache.
func (c *CachedEpochValidator) CurrentEpochForToken(ctx context.Context, playerID, tokenEpoch int64) (int64, error) {
	return c.currentEpoch(ctx, playerID, &tokenEpoch)
}

func (c *CachedEpochValidator) currentEpoch(ctx context.Context, playerID int64, tokenEpoch *int64) (int64, error) {
	if c.ttl <= 0 {
		c.record("disabled")
		return c.inner.CurrentEpoch(ctx, playerID)
	}

	now := c.now()
	c.mu.Lock()
	healthy := c.healthy
	entry, found := c.entries[playerID]
	generation := c.generation
	c.mu.Unlock()
	if !healthy {
		c.record("bypass")
		return c.inner.CurrentEpoch(ctx, playerID)
	}
	if found && now.Before(entry.expiresAt) && (tokenEpoch == nil || *tokenEpoch <= entry.epoch) {
		c.record("hit")
		return entry.epoch, nil
	}
	if found && tokenEpoch != nil && *tokenEpoch > entry.epoch {
		c.record("reload")
	} else {
		c.record("miss")
	}

	epoch, err := c.inner.CurrentEpoch(ctx, playerID)
	if err != nil {
		c.record("error")
		return 0, err
	}
	c.mu.Lock()
	if c.healthy && c.generation == generation {
		if _, exists := c.entries[playerID]; !exists && len(c.entries) >= sessionEpochCacheMaxEntries {
			for id := range c.entries {
				delete(c.entries, id)
				break
			}
		}
		c.entries[playerID] = epochCacheEntry{epoch: epoch, expiresAt: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	return epoch, nil
}

func (c *CachedEpochValidator) record(result string) {
	if c.requests != nil {
		c.requests.WithLabelValues(result).Inc()
	}
}
