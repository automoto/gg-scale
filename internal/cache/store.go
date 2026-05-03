// Package cache is the vendor-neutral interface to the fast tier (rate-limit
// buckets, connection-cap counters, short-lived memoised values).
//
// Implementations live in subpackages: memory (in-process, single node) and
// olric (embedded or client-mode Olric cluster). Call sites depend only on
// the Store interface here.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("cache: key not found")

// Store is the data-plane abstraction backed by an in-memory or distributed
// key/value store. All methods are safe for concurrent use.
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

	// ReleaseSlot decrements the counter at key, clamped to zero (a release
	// without a matching acquire is a no-op rather than going negative).
	ReleaseSlot(ctx context.Context, key string) error

	// RefreshSlot extends the counter's idle TTL without changing the
	// value. Safe to call from a heartbeat goroutine on every active
	// session; idempotent.
	RefreshSlot(ctx context.Context, key string, ttl time.Duration) error

	// Get returns the value at key. ErrNotFound when absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set writes value at key with the given TTL. ttl=0 means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes key from all cache namespaces. Missing keys are not an error.
	Delete(ctx context.Context, key string) error

	// Close releases backend resources. Implementations of clustered
	// backends (Olric) trigger a clean leave/shutdown.
	Close(ctx context.Context) error
}
