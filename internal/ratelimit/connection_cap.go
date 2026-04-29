package ratelimit

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/ggscale/ggscale/internal/tenant"
)

// CloseCodeTenantConnectionCap is the WebSocket close code used when a new
// connection would exceed the tenant's concurrency cap. 1013 ("Try Again
// Later") is RFC 6455 §7.4.1's intended use — the SDK is expected to
// retry with backoff.
const CloseCodeTenantConnectionCap = 1013

// CloseReasonTenantConnectionCap is the close-frame reason string the
// Phase-2 WebSocket handler will send alongside CloseCodeTenantConnectionCap.
// Documented here so the SDK can rely on a stable value.
const CloseReasonTenantConnectionCap = "tenant_connection_cap"

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
// before upgrading. Acquire INCRs the per-key counter and rejects (rolling
// back) when the new value would exceed the supplied limit. Release DECRs
// on close.
type ConnectionCap interface {
	Acquire(ctx context.Context, key string, limit int64) (CapDecision, error)
	Release(ctx context.Context, key string) error
}

// luaAcquireCap atomically INCRs and rejects-with-rollback above the cap.
// Sets a long TTL on the counter so a process crash that misses Release
// calls doesn't leak the counter forever.
//
// KEYS: counter
// ARGV: limit, ttl_seconds
// Returns: { allowed (0|1), current }
const luaAcquireCap = `
local key   = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl   = tonumber(ARGV[2])

local current = redis.call('INCR', key)
if current > limit then
    redis.call('DECR', key)
    return {0, current - 1}
end
redis.call('EXPIRE', key, ttl)
return {1, current}
`

// counterIdleTTL bounds how long a stale counter survives without any
// activity (no Acquires, no Releases). Six hours is well past any
// reasonable WebSocket session, so an actual leak only happens if the
// server crashes mid-session AND no other connections come in for that
// tenant within the window.
//
// TODO(phase2): the current TTL+DECR pattern is unsafe for WebSockets
// that stay open longer than counterIdleTTL with no other Acquire
// traffic for the same tenant. When that happens the key expires; the
// eventual Release issues a DECR against a missing key, leaving the
// counter at -1 and silently disabling the cap. The Phase-2 WS handler
// must:
//
//	(a) refresh the counter's TTL on every heartbeat from any active
//	    connection for that tenant (cheap PEXPIRE), and
//	(b) clamp Release in Lua so DECR never drops below zero
//	    (e.g., `if redis.call('GET', KEYS[1]) > 0 then DECR end`).
//
// Tracked in the 2026-04-27 backend code review (item 2).
const counterIdleTTL = 6 * 3600

// ValkeyConnectionCap is a ConnectionCap backed by Valkey.
type ValkeyConnectionCap struct {
	client   *redis.Client
	acquireS *redis.Script
}

// NewValkeyConnectionCap constructs a ValkeyConnectionCap. The Lua script
// is uploaded lazily on the first Acquire (EVALSHA fallback to EVAL).
func NewValkeyConnectionCap(client *redis.Client) *ValkeyConnectionCap {
	return &ValkeyConnectionCap{
		client:   client,
		acquireS: redis.NewScript(luaAcquireCap),
	}
}

// Acquire attempts to reserve one connection slot under key. On success
// the caller must call Release exactly once when the connection closes.
func (c *ValkeyConnectionCap) Acquire(ctx context.Context, key string, limit int64) (CapDecision, error) {
	res, err := c.acquireS.Run(ctx, c.client, []string{key}, limit, counterIdleTTL).Result()
	if err != nil {
		return CapDecision{}, fmt.Errorf("connection cap script: %w", err)
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return CapDecision{}, fmt.Errorf("connection cap script: unexpected reply %#v", res)
	}
	allowed, _ := arr[0].(int64)
	current, _ := arr[1].(int64)

	return CapDecision{Allowed: allowed == 1, Current: current}, nil
}

// Release decrements the counter under key. Safe to call once per
// successful Acquire; double-releases or releases without a matching
// Acquire will under-count and let extra connections through.
func (c *ValkeyConnectionCap) Release(ctx context.Context, key string) error {
	if err := c.client.Decr(ctx, key).Err(); err != nil {
		return fmt.Errorf("connection cap release: %w", err)
	}
	return nil
}

// ConnectionCapKey is the canonical Valkey key for a tenant's WebSocket
// counter. Centralised so the Phase-2 WS handler and any
// observability/admin tooling agree on the key shape.
func ConnectionCapKey(tenantID int64) string {
	return fmt.Sprintf("wsconn:%d", tenantID)
}
