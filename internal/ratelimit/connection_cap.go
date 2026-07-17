package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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

const cacheConnectionCapTTL = 6 * time.Hour

// ConnectionBurstBudget is how much full-2× wall time a tenant may sustain
// above its sustained cap before the burst bucket clamps it back. Reconnect
// storms, launch evenings, and streamer raids fit; camping at 2× all day does
// not. The budget refills over cache.BurstRefillWindow at/below sustained.
const ConnectionBurstBudget = 10 * time.Minute

// CapLimits is a class's connection envelope: the sustained cap that is always
// available and the hard ceiling (2× sustained) reachable only while burst
// budget remains.
type CapLimits struct {
	Sustained int64
	Ceiling   int64
}

// ConnectionCapForClass maps a tenant class to its connection envelope. CCU is
// deliberately generous everywhere — idle sockets cost ~nothing; projects,
// storage, and registered players are the upgrade levers. tier_3 is the
// operator's starting point before per-axis overrides. Unknown/out-of-range
// classes fall back to tier_0 — fail-closed.
func ConnectionCapForClass(t tenant.Tier) CapLimits {
	var sustained int64
	switch t {
	case tenant.Tier1:
		sustained = 20000
	case tenant.Tier2:
		sustained = 50000
	case tenant.Tier3:
		sustained = 50000
	default:
		sustained = 5000
	}
	return CapLimits{Sustained: sustained, Ceiling: 2 * sustained}
}

// Reject reasons distinguish which wall a connection hit, so operators can see
// whether tenants are camping at the ceiling or burning their burst budget.
const (
	CapRejectCeiling     = "ceiling"
	CapRejectBudget      = "budget"
	CapRejectUnavailable = "unavailable"
)

// CapDecision is the outcome of a single Acquire call.
type CapDecision struct {
	Allowed bool
	// Current is the post-decision counter value. Useful for logging and
	// emitting "X-Open-Connections" headers.
	Current int64
	// Reason is set on rejection to CapRejectCeiling, CapRejectBudget, or
	// CapRejectUnavailable.
	Reason string
	// Emergency reports an admission made from the bounded process-local
	// allowance while the shared grant store was unavailable.
	Emergency bool
}

// ConnectionCap is the interface the WebSocket handler calls before upgrade.
// Implementations keep long-lived reservation renewal out of socket heartbeats.
type ConnectionCap interface {
	Acquire(ctx context.Context, tenantID int64, caps CapLimits) (CapDecision, error)
	Release(ctx context.Context, tenantID int64) error
}

// CacheConnectionCap is the single-process ConnectionCap implementation used
// by focused tests. Managed production uses LeasedConnectionCap.
type CacheConnectionCap struct {
	store      cache.Store
	rejections *prometheus.CounterVec
}

// NewCacheConnectionCap wraps store as a ConnectionCap. When reg is non-nil it
// registers a rejection counter labelled by reason (ceiling vs budget).
func NewCacheConnectionCap(store cache.Store, reg prometheus.Registerer) *CacheConnectionCap {
	c := &CacheConnectionCap{store: store}
	if reg != nil {
		c.rejections = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_connection_cap_rejections_total",
			Help: "WebSocket connections rejected by the tenant CCU cap, by reason (ceiling vs exhausted burst budget).",
		}, []string{"reason"})
		reg.MustRegister(c.rejections)
	}
	return c
}

// Acquire reserves one slot within the class burst envelope. On success the
// caller must invoke Release exactly once.
func (c *CacheConnectionCap) Acquire(ctx context.Context, tenantID int64, caps CapLimits) (CapDecision, error) {
	key := ConnectionCapKey(tenantID)
	ok, current, err := c.store.AcquireSlotBurst(ctx, key, caps.Sustained, caps.Ceiling, ConnectionBurstBudget, cacheConnectionCapTTL)
	if err != nil {
		return CapDecision{}, fmt.Errorf("connection cap acquire: %w", err)
	}
	d := CapDecision{Allowed: ok, Current: current}
	if !ok {
		d.Reason = CapRejectBudget
		if current >= caps.Ceiling {
			d.Reason = CapRejectCeiling
		}
		if c.rejections != nil {
			c.rejections.WithLabelValues(d.Reason).Inc()
		}
	}
	return d, nil
}

// Release decrements the counter under key, clamped at zero so a spurious
// double-release cannot drive the counter negative.
func (c *CacheConnectionCap) Release(ctx context.Context, tenantID int64) error {
	key := ConnectionCapKey(tenantID)
	if err := c.store.ReleaseSlotBurst(ctx, key); err != nil {
		return fmt.Errorf("connection cap release: %w", err)
	}
	return nil
}

// ConnectionCapKey is the canonical cache key for a tenant's WebSocket
// counter. Centralised so the Phase-2 WS handler and any
// observability/admin tooling agree on the key shape.
func ConnectionCapKey(tenantID int64) string {
	return fmt.Sprintf("wsconn:%d", tenantID)
}
