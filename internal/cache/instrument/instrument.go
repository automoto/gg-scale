// Package instrument wraps a cache.Store with Prometheus counters.
// Mount via build.Config.Registry so the same label shape
// (op, result) works across the memory and olric backends.
package instrument

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/cache"
)

// Store wraps any cache.Store and records ggscale_cache_ops_total{op,result}.
type Store struct {
	next    cache.Store
	counter *prometheus.CounterVec
}

// New registers ggscale_cache_ops_total on reg and returns an instrumented
// wrapper around next.
func New(next cache.Store, reg prometheus.Registerer) *Store {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ggscale_cache_ops_total",
		Help: "Total cache operations partitioned by op and result.",
	}, []string{"op", "result"})
	reg.MustRegister(c)
	return &Store{next: next, counter: c}
}

func (s *Store) inc(op, result string) {
	s.counter.WithLabelValues(op, result).Inc()
}

// TokenBucket implements cache.Store.
func (s *Store) TokenBucket(ctx context.Context, key string, capacity, refillPerSec, cost float64) (bool, time.Duration, error) {
	allowed, retry, err := s.next.TokenBucket(ctx, key, capacity, refillPerSec, cost)
	switch {
	case err != nil:
		s.inc("token_bucket", "error")
	case allowed:
		s.inc("token_bucket", "ok")
	default:
		s.inc("token_bucket", "denied")
	}
	return allowed, retry, err
}

// AcquireSlot implements cache.Store.
func (s *Store) AcquireSlot(ctx context.Context, key string, limit int64, ttl time.Duration) (bool, int64, error) {
	acquired, current, err := s.next.AcquireSlot(ctx, key, limit, ttl)
	switch {
	case err != nil:
		s.inc("acquire_slot", "error")
	case acquired:
		s.inc("acquire_slot", "ok")
	default:
		s.inc("acquire_slot", "rejected")
	}
	return acquired, current, err
}

// ReleaseSlot implements cache.Store.
func (s *Store) ReleaseSlot(ctx context.Context, key string) error {
	err := s.next.ReleaseSlot(ctx, key)
	if err != nil {
		s.inc("release_slot", "error")
	} else {
		s.inc("release_slot", "ok")
	}
	return err
}

// RefreshSlot implements cache.Store.
func (s *Store) RefreshSlot(ctx context.Context, key string, ttl time.Duration) error {
	err := s.next.RefreshSlot(ctx, key, ttl)
	if err != nil {
		s.inc("refresh_slot", "error")
	} else {
		s.inc("refresh_slot", "ok")
	}
	return err
}

// Get implements cache.Store. Distinguishes hit, miss, and error results.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.next.Get(ctx, key)
	switch {
	case err == nil:
		s.inc("get", "hit")
	case errors.Is(err, cache.ErrNotFound):
		s.inc("get", "miss")
	default:
		s.inc("get", "error")
	}
	return v, err
}

// Set implements cache.Store.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	err := s.next.Set(ctx, key, value, ttl)
	if err != nil {
		s.inc("set", "error")
	} else {
		s.inc("set", "ok")
	}
	return err
}

// Delete implements cache.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	err := s.next.Delete(ctx, key)
	if err != nil {
		s.inc("delete", "error")
	} else {
		s.inc("delete", "ok")
	}
	return err
}

// Close implements cache.Store. Delegates to the underlying store.
func (s *Store) Close(ctx context.Context) error {
	return s.next.Close(ctx)
}
