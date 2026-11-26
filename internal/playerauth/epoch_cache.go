package playerauth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/automoto/gg-scale/internal/cacheinvalidation"
	"github.com/automoto/gg-scale/internal/db"
)

const (
	// DefaultEpochCacheTTL matches the API-key token cache default. A ban or
	// other epoch write evicts immediately instead of waiting for this TTL.
	DefaultEpochCacheTTL = 30 * time.Second
	epochCacheMaxEntries = 10000
)

type epochCacheEntry struct {
	tenantID int64
	epoch    int64
	revoked  bool
	expires  time.Time
}

// CachedEpochValidator memoizes epoch and revoked lookups in process memory.
// Other backend errors are never cached. Eviction increments drops so a read
// that was in flight during a write cannot repopulate a stale entry.
type CachedEpochValidator struct {
	inner    EpochValidator
	ttl      time.Duration
	now      func() time.Time
	notifier cacheinvalidation.Notifier

	mu      sync.Mutex
	entries map[int64]epochCacheEntry
	drops   uint64
}

// NewCachedEpochValidator wraps inner with a 30-second cache by default.
func NewCachedEpochValidator(inner EpochValidator, ttl time.Duration) *CachedEpochValidator {
	if ttl <= 0 {
		ttl = DefaultEpochCacheTTL
	}
	return &CachedEpochValidator{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[int64]epochCacheEntry),
	}
}

// WithNotifier configures cross-instance eviction for Invalidate and
// InvalidateTenant. Set it during startup, before serving requests.
func (c *CachedEpochValidator) WithNotifier(notifier cacheinvalidation.Notifier) *CachedEpochValidator {
	c.notifier = notifier
	return c
}

// CurrentEpoch implements EpochValidator.
func (c *CachedEpochValidator) CurrentEpoch(ctx context.Context, playerID int64) (int64, error) {
	tenantID, _ := db.TenantFromContext(ctx)
	now := c.now()
	c.mu.Lock()
	entry, ok := c.entries[playerID]
	drops := c.drops
	c.mu.Unlock()
	if ok && now.Before(entry.expires) {
		if entry.revoked {
			return 0, ErrRevoked
		}
		return entry.epoch, nil
	}

	epoch, err := c.inner.CurrentEpoch(ctx, playerID)
	if err != nil && !errors.Is(err, ErrRevoked) {
		return 0, err
	}
	c.mu.Lock()
	if c.drops != drops {
		c.mu.Unlock()
		return epoch, err
	}
	if _, exists := c.entries[playerID]; !exists && len(c.entries) >= epochCacheMaxEntries {
		for id := range c.entries {
			delete(c.entries, id)
			break
		}
	}
	c.entries[playerID] = epochCacheEntry{
		tenantID: tenantID,
		epoch:    epoch,
		revoked:  errors.Is(err, ErrRevoked),
		expires:  now.Add(c.ttl),
	}
	c.mu.Unlock()
	return epoch, err
}

// Evict drops one player on this process only.
func (c *CachedEpochValidator) Evict(playerID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, playerID)
	c.drops++
	c.mu.Unlock()
}

// EvictTenant drops every cached player of a tenant on this process only.
func (c *CachedEpochValidator) EvictTenant(tenantID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for playerID, entry := range c.entries {
		if entry.tenantID == tenantID {
			delete(c.entries, playerID)
		}
	}
	c.drops++
	c.mu.Unlock()
}

// Invalidate evicts one player locally, then notifies the other instances.
func (c *CachedEpochValidator) Invalidate(ctx context.Context, playerID int64) {
	if c == nil {
		return
	}
	c.Evict(playerID)
	c.notify(ctx, cacheinvalidation.Event{Kind: cacheinvalidation.KindPlayerEpoch, PlayerID: playerID})
}

// InvalidateTenant evicts all of a tenant's players locally, then notifies the
// other instances. Tenant-wide bans use this because one account can link to
// several project-player rows.
func (c *CachedEpochValidator) InvalidateTenant(ctx context.Context, tenantID int64) {
	if c == nil {
		return
	}
	c.EvictTenant(tenantID)
	c.notify(ctx, cacheinvalidation.Event{Kind: cacheinvalidation.KindTenantEpochs, TenantID: tenantID})
}

// InvalidateAll is the local fallback used after a missed notification.
func (c *CachedEpochValidator) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.entries)
	c.drops++
	c.mu.Unlock()
}

func (c *CachedEpochValidator) notify(ctx context.Context, event cacheinvalidation.Event) {
	if c.notifier == nil {
		return
	}
	if err := cacheinvalidation.Notify(ctx, c.notifier, event); err != nil {
		slog.WarnContext(ctx, "session epoch cache notification failed; TTL and fallback remain active", "error", err)
	}
}
