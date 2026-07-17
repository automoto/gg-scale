// Package cache defines the process-local fast tier for rate-limit buckets,
// per-player connection counters, and short-lived memoised values. Shared
// correctness belongs in PostgreSQL.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("cache: key not found")

// Store is the process-local cache abstraction. All methods are safe for
// concurrent use.
type Store interface {
	// TokenBucket consumes cost tokens from the bucket at key. capacity is
	// the burst (and the initial fill); refillPerSec is the steady-state
	// rate. allowed=true means the request fits and tokens were debited;
	// allowed=false means the bucket is exhausted and retryAfter is the
	// time until cost tokens will be available.
	//
	// Implementations must be atomic per-key but tolerate cross-key races.
	TokenBucket(ctx context.Context, key string, capacity, refillPerSec, cost float64) (allowed bool, retryAfter time.Duration, err error)

	// AcquireSlot atomically reserves one slot in the counter at key,
	// rejecting (and rolling back) if the post-increment value would
	// exceed limit. ttl bounds how long an idle counter survives without
	// further activity; callers should call RefreshSlot periodically on
	// long-lived holders to prevent the counter from expiring underfoot.
	//
	// current is the post-decision counter value (the limit on rejection).
	AcquireSlot(ctx context.Context, key string, limit int64, ttl time.Duration) (acquired bool, current int64, err error)

	// AcquireSlotBurst reserves one slot under a burst model: connections up
	// to sustained are always admitted; connections between sustained and
	// ceiling are admitted only while burst budget remains; ceiling is a hard
	// wall. The budget (burstBudget of full-2× wall time) drains above
	// sustained and refills at/below it over BurstRefillWindow. ttl bounds idle
	// survival as with AcquireSlot. See AdmitBurst for the exact semantics.
	//
	// current is the connection count after the decision; on rejection it is
	// the unchanged count, so a caller can tell a ceiling rejection
	// (current >= ceiling) from a budget-exhaustion one (current < ceiling).
	AcquireSlotBurst(ctx context.Context, key string, sustained, ceiling int64, burstBudget, ttl time.Duration) (acquired bool, current int64, err error)

	// ReleaseSlot decrements the plain counter at key (AcquireSlot), clamped
	// to zero (a release without a matching acquire is a no-op rather than
	// going negative). It does not touch burst slots — use ReleaseSlotBurst
	// for those.
	ReleaseSlot(ctx context.Context, key string) error

	// RefreshSlot extends a plain counter's idle TTL without changing the
	// value. Safe to call from a heartbeat goroutine on every active
	// session; idempotent. It does not touch burst slots.
	RefreshSlot(ctx context.Context, key string, ttl time.Duration) error

	// ReleaseSlotBurst decrements a burst counter (AcquireSlotBurst) at key,
	// clamped to zero. The burst counterpart to ReleaseSlot; kept separate so
	// plain and burst counters never depend on sharing a key namespace.
	ReleaseSlotBurst(ctx context.Context, key string) error

	// RefreshSlotBurst extends a live burst counter's idle TTL. It is a no-op
	// on an absent or already-expired slot, so a stray refresh cannot
	// resurrect a reaped counter. Safe to call from a heartbeat goroutine.
	RefreshSlotBurst(ctx context.Context, key string, ttl time.Duration) error

	// Get returns the value at key. ErrNotFound when absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set writes value at key with the given TTL. ttl=0 means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key from all cache namespaces. Missing keys are not an error.
	Delete(ctx context.Context, key string) error

	// Close stops background cleanup and releases backend resources.
	Close(ctx context.Context) error
}
