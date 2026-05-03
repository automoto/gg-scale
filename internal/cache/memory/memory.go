// Package memory is the in-process cache.Store backend. Suitable for
// single-node self-host and as the unit-test backend for handlers that
// depend on a Store. Multi-node deployments use the olric backend.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/ggscale/ggscale/internal/cache"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type slot struct {
	count   int64
	expires time.Time
}

type kvEntry struct {
	value   []byte
	expires time.Time
}

// Store is a sync.Mutex-guarded in-memory implementation of cache.Store.
type Store struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	slots   map[string]*slot
	kv      map[string]*kvEntry
	now     func() time.Time
}

// New returns a fresh in-memory Store.
func New() *Store {
	return &Store{
		buckets: make(map[string]*bucket),
		slots:   make(map[string]*slot),
		kv:      make(map[string]*kvEntry),
		now:     time.Now,
	}
}

// TokenBucket implements cache.Store.
func (s *Store) TokenBucket(_ context.Context, key string, capacity, refillPerSec, cost float64) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		s.buckets[key] = b
	}

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(capacity, b.tokens+elapsed*refillPerSec)
	}
	b.last = now

	if b.tokens < cost {
		missing := cost - b.tokens
		retry := time.Duration(missing / refillPerSec * float64(time.Second))
		return false, retry, nil
	}
	b.tokens -= cost
	return true, 0, nil
}

// AcquireSlot implements cache.Store.
func (s *Store) AcquireSlot(_ context.Context, key string, limit int64, ttl time.Duration) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	sl, ok := s.slots[key]
	if !ok || sl.expires.Before(now) {
		sl = &slot{}
		s.slots[key] = sl
	}

	if sl.count+1 > limit {
		return false, limit, nil
	}
	sl.count++
	sl.expires = now.Add(ttl)
	return true, sl.count, nil
}

// ReleaseSlot implements cache.Store.
func (s *Store) ReleaseSlot(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sl, ok := s.slots[key]
	if !ok {
		return nil
	}
	if sl.count > 0 {
		sl.count--
	}
	return nil
}

// RefreshSlot implements cache.Store.
func (s *Store) RefreshSlot(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sl, ok := s.slots[key]
	if !ok {
		return nil
	}
	sl.expires = s.now().Add(ttl)
	return nil
}

// Get implements cache.Store.
func (s *Store) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.kv[key]
	if !ok {
		return nil, cache.ErrNotFound
	}
	if !e.expires.IsZero() && e.expires.Before(s.now()) {
		delete(s.kv, key)
		return nil, cache.ErrNotFound
	}
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

// Set implements cache.Store.
func (s *Store) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored := make([]byte, len(value))
	copy(stored, value)

	e := &kvEntry{value: stored}
	if ttl > 0 {
		e.expires = s.now().Add(ttl)
	}
	s.kv[key] = e
	return nil
}

// Delete implements cache.Store.
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.buckets, key)
	delete(s.slots, key)
	delete(s.kv, key)
	return nil
}

// Close implements cache.Store. Memory backend has nothing to release.
func (s *Store) Close(_ context.Context) error { return nil }
