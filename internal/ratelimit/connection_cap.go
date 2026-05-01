package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/tenant"
)

// CloseCodeTenantConnectionCap is the WebSocket close code used when a new
// connection would exceed the tenant's concurrency cap. 1013 ("Try Again
// Later") is RFC 6455 §7.4.1's intended use — the SDK is expected to
// retry with backoff.
const CloseCodeTenantConnectionCap = 1013

// CloseReasonTenantConnectionCap is the close-frame reason string the
// Phase-2 WebSocket handler will send alongside CloseCodeTenantConnectionCap.
const CloseReasonTenantConnectionCap = "tenant_connection_cap"

// ConnectionCapIdleTTL is how long an idle counter survives without any
// Acquire/Refresh activity. Long-lived holders must call Refresh from a
// heartbeat goroutine to keep the counter alive past this window.
const ConnectionCapIdleTTL = 6 * time.Hour

// ConnectionCapHeartbeat is the recommended interval for callers to invoke
// Refresh on each held slot. Comfortably under ConnectionCapIdleTTL with
// margin for one missed tick.
const ConnectionCapHeartbeat = 30 * time.Minute

// ConnectionCapForTier maps a tenant tier to the maximum number of
// concurrent WebSocket connections it can hold open. Per-tenant Premium
// overrides land alongside the rate-limit override (v1.1).
func ConnectionCapForTier(t tenant.Tier) int64 {
	switch t {
	case tenant.TierFree:
		return 100
	case tenant.TierPAYG:
		return 1000
	case tenant.TierPremium:
		return 10000
	default:
		return 100
	}
}

// CapDecision is the outcome of a single Acquire call.
type CapDecision struct {
	Allowed bool
	// Current is the post-decision counter value. Useful for logging and
	// emitting "X-Open-Connections" headers.
	Current int64
}

// ConnectionCap is the interface the Phase-2 WebSocket handler will call
// before upgrading. Acquire reserves one slot under key (rejecting at the
// per-tenant cap); Release decrements on close; Refresh is called by the
// caller's heartbeat to keep the counter from expiring under long-lived
// connections.
type ConnectionCap interface {
	Acquire(ctx context.Context, key string, limit int64) (CapDecision, error)
	Release(ctx context.Context, key string) error
	Refresh(ctx context.Context, key string) error
}

// CacheConnectionCap implements ConnectionCap on a cache.Store. Counter
// state lives in the slots DMap of the configured backend.
type CacheConnectionCap struct {
	store cache.Store
}

// NewCacheConnectionCap wraps store as a ConnectionCap.
func NewCacheConnectionCap(store cache.Store) *CacheConnectionCap {
	return &CacheConnectionCap{store: store}
}

// Acquire reserves one slot under key. On success the caller must invoke
// Release exactly once and Refresh periodically (every
// ConnectionCapHeartbeat) until close.
func (c *CacheConnectionCap) Acquire(ctx context.Context, key string, limit int64) (CapDecision, error) {
	ok, current, err := c.store.AcquireSlot(ctx, key, limit, ConnectionCapIdleTTL)
	if err != nil {
		return CapDecision{}, fmt.Errorf("connection cap acquire: %w", err)
	}
	return CapDecision{Allowed: ok, Current: current}, nil
}

// Release decrements the counter under key, clamped at zero so a spurious
// double-release cannot drive the counter negative.
func (c *CacheConnectionCap) Release(ctx context.Context, key string) error {
	if err := c.store.ReleaseSlot(ctx, key); err != nil {
		return fmt.Errorf("connection cap release: %w", err)
	}
	return nil
}

// Refresh extends the counter's idle TTL. Safe to call on every heartbeat
// from any active connection for the tenant; idempotent.
func (c *CacheConnectionCap) Refresh(ctx context.Context, key string) error {
	if err := c.store.RefreshSlot(ctx, key, ConnectionCapIdleTTL); err != nil {
		return fmt.Errorf("connection cap refresh: %w", err)
	}
	return nil
}

// ConnectionCapKey is the canonical cache key for a tenant's WebSocket
// counter. Centralised so the Phase-2 WS handler and any
// observability/admin tooling agree on the key shape.
func ConnectionCapKey(tenantID int64) string {
	return fmt.Sprintf("wsconn:%d", tenantID)
}
