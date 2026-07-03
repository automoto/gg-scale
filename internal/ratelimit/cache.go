package ratelimit

import (
	"context"
	"fmt"

	"github.com/ggscale/ggscale/internal/cache"
)

// CacheLimiter implements Limiter on top of a cache.Store. The bucket math
// lives in the Store backend (memory or olric); this type just adapts the
// HTTP-shaped Decision return value.
type CacheLimiter struct {
	store cache.Store
}

// NewCacheLimiter wraps store as a Limiter.
func NewCacheLimiter(store cache.Store) *CacheLimiter {
	return &CacheLimiter{store: store}
}

// Allow consults the bucket for key. Each call debits one token.
func (l *CacheLimiter) Allow(ctx context.Context, key string, ratePerSecond, burst float64) (Decision, error) {
	allowed, retry, err := l.store.TokenBucket(ctx, key, burst, ratePerSecond, 1)
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: token bucket: %w", err)
	}
	return Decision{Allowed: allowed, RetryAfter: retry}, nil
}

// Refund credits one token back to the bucket (a negative-cost token-bucket
// call). The backend caps the result at capacity, so a refund can never lift a
// bucket above its burst. Used to undo a debit when the guarded action fails.
func (l *CacheLimiter) Refund(ctx context.Context, key string, ratePerSecond, burst float64) error {
	if _, _, err := l.store.TokenBucket(ctx, key, burst, ratePerSecond, -1); err != nil {
		return fmt.Errorf("ratelimit: token bucket refund: %w", err)
	}
	return nil
}
