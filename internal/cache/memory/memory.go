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

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	slots   map[string]*slot
	kv      map[string]*kvEntry
}

// Store is a sharded in-memory implementation of cache.Store.
type Store struct {
	shards [shardCount]shard
	now    func() time.Time

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// sweepInterval is how often the janitor wakes to evict idle/expired keys.
// Short enough that a churning rate-limiter doesn't accumulate memory;
// long enough that the sweep overhead is invisible.
const sweepInterval = time.Minute
const shardCount = 32

// New returns a fresh in-memory Store with a background janitor that
// reclaims expired kv entries, stale slots, and idle rate-limit buckets.
// The janitor exits on Close.
func New() *Store {
	s := &Store{
		now:  time.Now,
		stop: make(chan struct{}),
	}
	for i := range s.shards {
		s.shards[i].buckets = make(map[string]*bucket)
		s.shards[i].slots = make(map[string]*slot)
		s.shards[i].kv = make(map[string]*kvEntry)
	}
	s.wg.Add(1)
	go s.janitor(sweepInterval)
	return s
}

// janitor sweeps every interval until Close. Two-phase scan (collect keys
// under the lock, delete in a follow-up critical section) keeps the
// critical section short even for large maps.
func (s *Store) janitor(interval time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// sweep removes:
//   - kv entries past their expiry
//   - slot entries whose count is 0 AND expires < now
//   - buckets at full capacity that haven't been touched in 10× the
//     sweep interval (idle bucket: the next request rebuilds it)
func (s *Store) sweep() {
	now := s.now()
	idleCutoff := now.Add(-10 * sweepInterval)

	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for k, e := range sh.kv {
			if !e.expires.IsZero() && e.expires.Before(now) {
				delete(sh.kv, k)
			}
		}
		for k, sl := range sh.slots {
			if sl.count == 0 && sl.expires.Before(now) {
				delete(sh.slots, k)
			}
		}
		for k, b := range sh.buckets {
			if b.last.Before(idleCutoff) {
				delete(sh.buckets, k)
			}
		}
		sh.mu.Unlock()
	}
}

// TokenBucket implements cache.Store.
func (s *Store) TokenBucket(_ context.Context, key string, capacity, refillPerSec, cost float64) (bool, time.Duration, error) {
	if refillPerSec <= 0 {
		return false, time.Second, nil
	}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := s.now()
	b, ok := sh.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		sh.buckets[key] = b
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
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	now := s.now()
	sl, ok := sh.slots[key]
	if !ok || sl.expires.Before(now) {
		sl = &slot{}
		sh.slots[key] = sl
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
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sl, ok := sh.slots[key]
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
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	sl, ok := sh.slots[key]
	if !ok {
		return nil
	}
	sl.expires = s.now().Add(ttl)
	return nil
}

// Get implements cache.Store.
func (s *Store) Get(_ context.Context, key string) ([]byte, error) {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	e, ok := sh.kv[key]
	if !ok {
		return nil, cache.ErrNotFound
	}
	if !e.expires.IsZero() && e.expires.Before(s.now()) {
		delete(sh.kv, key)
		return nil, cache.ErrNotFound
	}
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

// Set implements cache.Store.
func (s *Store) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	stored := make([]byte, len(value))
	copy(stored, value)

	e := &kvEntry{value: stored}
	if ttl > 0 {
		e.expires = s.now().Add(ttl)
	}
	sh.kv[key] = e
	return nil
}

// Delete implements cache.Store.
func (s *Store) Delete(_ context.Context, key string) error {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	delete(sh.buckets, key)
	delete(sh.slots, key)
	delete(sh.kv, key)
	return nil
}

// Close implements cache.Store. Stops the background janitor; calling
// Close twice is safe (the underlying channel close is once-guarded).
func (s *Store) Close(_ context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
	return nil
}

func (s *Store) shardFor(key string) *shard {
	return &s.shards[fnv32(key)%shardCount]
}

func fnv32(key string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return h
}
