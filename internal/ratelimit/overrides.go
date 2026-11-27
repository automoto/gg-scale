package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// newOverrideErrorCounter registers (idempotently) the counter tracking
// override-store lookups that errored and fell back to the compiled default.
// A rising count means a configured (possibly tightened) override is silently
// not being applied — visible instead of failing silently. path distinguishes
// the "api" limiter from the "invite" throttle.
func newOverrideErrorCounter(reg prometheus.Registerer) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ggscale_ratelimit_override_error_total",
		Help: "Override-store lookups that errored and fell back to the compiled default limit.",
	}, []string{"path"})
	if err := reg.Register(c); err != nil {
		are, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		c = are.ExistingCollector.(*prometheus.CounterVec)
	}
	return c
}

// Override kinds persisted in rate_limit_overrides.kind.
const (
	OverrideKindAPI             = "api"
	OverrideKindInviteInviter   = "invite_inviter"
	OverrideKindInviteDomain    = "invite_domain"
	OverrideKindInviteRecipient = "invite_recipient"
)

// OverrideStore resolves persisted per-tenant / per-project rate-limit
// overrides. A nil OverrideStore means "defaults only".
type OverrideStore interface {
	// APILimit returns the tenant's HTTP API override; ok=false falls back to
	// the compiled tier default.
	APILimit(ctx context.Context, tenantID int64) (Limits, bool, error)
	// InviteLimit returns the most-specific (project-then-tenant) override for
	// an invite kind; ok=false falls back to the default invite limits.
	InviteLimit(ctx context.Context, tenantID, projectID int64, kind string) (Limits, bool, error)
}

// DBOverrideStore reads rate_limit_overrides from Postgres via BootstrapQ (the
// table is platform-global with explicit tenant filtering, like feature_grants).
type DBOverrideStore struct {
	pool *db.Pool
}

// NewDBOverrideStore builds a Postgres-backed override store.
func NewDBOverrideStore(pool *db.Pool) *DBOverrideStore {
	return &DBOverrideStore{pool: pool}
}

// APILimit implements OverrideStore.
func (s *DBOverrideStore) APILimit(ctx context.Context, tenantID int64) (Limits, bool, error) {
	var out Limits
	found := false
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetAPIRateLimitOverride(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = Limits{RatePerSecond: row.Rate, Burst: row.Burst}
		found = true
		return nil
	})
	if err != nil {
		return Limits{}, false, fmt.Errorf("ratelimit: api override: %w", err)
	}
	return out, found, nil
}

// InviteLimit implements OverrideStore.
func (s *DBOverrideStore) InviteLimit(ctx context.Context, tenantID, projectID int64, kind string) (Limits, bool, error) {
	var out Limits
	found := false
	var projectFilter *int64
	if projectID > 0 {
		projectFilter = &projectID
	}
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).GetInviteRateLimitOverride(ctx, sqlcgen.GetInviteRateLimitOverrideParams{
			TenantID:  tenantID,
			Kind:      kind,
			ProjectID: projectFilter,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = Limits{RatePerSecond: row.Rate, Burst: row.Burst}
		found = true
		return nil
	})
	if err != nil {
		return Limits{}, false, fmt.Errorf("ratelimit: invite override: %w", err)
	}
	return out, found, nil
}

// OverrideInvalidator drops cached override entries for a tenant. A write path
// (the control panel override forms) calls this so a change takes effect without
// waiting out the cache TTL. It is separate from OverrideStore because only the
// caching layer implements it — a raw DBOverrideStore has nothing to drop.
type OverrideInvalidator interface {
	Invalidate(tenantID int64)
}

// CachedOverrideStore memoizes an OverrideStore for a short TTL so the
// per-request middleware doesn't hit Postgres on every call. Mirrors the
// feature-grant cache in internal/rbac.
//
// Freshness has two layers: LISTEN/NOTIFY drops a tenant's entries on every
// process, and the TTL is a fallback if a notification is missed. The listener
// calls SetHealthy so connected hosts use the configured TTL (60s by default)
// and disconnected hosts flush immediately and use a 5s fallback.
type CachedOverrideStore struct {
	inner OverrideStore
	ttl   time.Duration
	now   func() time.Time

	mu         sync.Mutex
	cache      map[string]overrideEntry
	healthy    bool
	generation uint64
}

type overrideEntry struct {
	limits    Limits
	found     bool
	expiresAt time.Time
}

// DefaultOverrideCacheTTL is the healthy default of
// RATE_LIMIT_OVERRIDE_CACHE_TTL. LISTEN/NOTIFY carries writes to other hosts,
// so this is only a fallback for a missed notification.
const DefaultOverrideCacheTTL = 60 * time.Second

// fallbackOverrideCacheTTL bounds staleness while the invalidation listener is
// disconnected. A disconnect also flushes every healthy entry at once.
const fallbackOverrideCacheTTL = 5 * time.Second

var _ OverrideInvalidator = (*CachedOverrideStore)(nil)

// NewCachedOverrideStore wraps inner with a TTL cache.
func NewCachedOverrideStore(inner OverrideStore, ttl time.Duration) *CachedOverrideStore {
	if ttl <= 0 {
		ttl = DefaultOverrideCacheTTL
	}
	return &CachedOverrideStore{
		inner: inner,
		ttl:   ttl,
		now:   time.Now,
		cache: make(map[string]overrideEntry),
	}
}

// SetHealthy selects the notification-backed or fallback TTL. Both transitions
// flush all entries so a reconnect cannot retain writes missed while down.
func (c *CachedOverrideStore) SetHealthy(healthy bool) {
	c.mu.Lock()
	c.healthy = healthy
	c.generation++
	clear(c.cache)
	c.mu.Unlock()
}

// InvalidateAll drops every tenant when the notification stream resets.
func (c *CachedOverrideStore) InvalidateAll() {
	c.mu.Lock()
	clear(c.cache)
	c.generation++
	c.mu.Unlock()
}

// APILimit implements OverrideStore with caching.
func (c *CachedOverrideStore) APILimit(ctx context.Context, tenantID int64) (Limits, bool, error) {
	key := "api:" + strconv.FormatInt(tenantID, 10)
	return c.get(key, func() (Limits, bool, error) { return c.inner.APILimit(ctx, tenantID) })
}

// InviteLimit implements OverrideStore with caching.
func (c *CachedOverrideStore) InviteLimit(ctx context.Context, tenantID, projectID int64, kind string) (Limits, bool, error) {
	key := "invite:" + strconv.FormatInt(tenantID, 10) + ":" + strconv.FormatInt(projectID, 10) + ":" + kind
	return c.get(key, func() (Limits, bool, error) { return c.inner.InviteLimit(ctx, tenantID, projectID, kind) })
}

// Invalidate drops every cached entry for a tenant — the tenant's API entry and
// all its per-project invite entries — so the next read reflects a just-written
// override. Coarse by design: writes are rare (admin config changes) and a
// tenant has only a handful of cached keys, so clearing all of them is cheaper
// than tracking exactly which projects/kinds a write touched.
func (c *CachedOverrideStore) Invalidate(tenantID int64) {
	id := strconv.FormatInt(tenantID, 10)
	apiKey := "api:" + id
	invitePrefix := "invite:" + id + ":"
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	delete(c.cache, apiKey)
	for k := range c.cache {
		if strings.HasPrefix(k, invitePrefix) {
			delete(c.cache, k)
		}
	}
}

func (c *CachedOverrideStore) get(key string, load func() (Limits, bool, error)) (Limits, bool, error) {
	now := c.now()
	c.mu.Lock()
	healthy := c.healthy
	generation := c.generation
	ttl := fallbackOverrideCacheTTL
	if healthy {
		ttl = c.ttl
	}
	if e, ok := c.cache[key]; ok && now.Before(e.expiresAt) {
		c.mu.Unlock()
		return e.limits, e.found, nil
	}
	c.mu.Unlock()

	limits, found, err := load()
	if err != nil {
		return Limits{}, false, err
	}
	c.mu.Lock()
	if c.generation == generation && c.healthy == healthy {
		c.cache[key] = overrideEntry{limits: limits, found: found, expiresAt: c.now().Add(ttl)}
	}
	c.mu.Unlock()
	return limits, found, nil
}
