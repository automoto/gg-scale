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

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
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
// (the dashboard override forms) calls this so a change takes effect without
// waiting out the cache TTL. It is separate from OverrideStore because only the
// caching layer implements it — a raw DBOverrideStore has nothing to drop.
type OverrideInvalidator interface {
	Invalidate(tenantID int64)
}

// CachedOverrideStore memoizes an OverrideStore for a short TTL so the
// per-request middleware doesn't hit Postgres on every call. Mirrors the
// feature-grant cache in internal/rbac.
//
// Freshness has two layers: the write path calls Invalidate to drop a tenant's
// entries immediately on the process that served the write, and the TTL bounds
// staleness everywhere else. The cache is per-process with no cross-node
// invalidation, so in a multi-process (olric) deployment other nodes still
// converge within DefaultOverrideCacheTTL rather than instantly.
type CachedOverrideStore struct {
	inner OverrideStore
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]overrideEntry
}

type overrideEntry struct {
	limits    Limits
	found     bool
	expiresAt time.Time
}

// DefaultOverrideCacheTTL matches the feature-grant cache freshness.
const DefaultOverrideCacheTTL = 5 * time.Second

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
	c.cache[key] = overrideEntry{limits: limits, found: found, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return limits, found, nil
}
