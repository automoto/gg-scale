// Package olric is a cache.Store backend on top of an embedded Olric node.
// Single-process deployments form a cluster of one (no peers, no gossip);
// multi-node deployments list peers in Config.Peers and Olric handles
// partitioning and replication.
package olric

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	olricpkg "github.com/olric-data/olric"
	olriccfg "github.com/olric-data/olric/config"

	"github.com/ggscale/ggscale/internal/cache"
)

// Config controls how the embedded Olric node is started.
type Config struct {
	// BindAddr is the Olric protocol bind address. Default: "127.0.0.1".
	BindAddr string
	// BindPort is the Olric protocol port. 0 means an ephemeral port; the
	// node logs the chosen port at startup. Default: 3320.
	BindPort int
	// MemberlistBindAddr is the gossip bind address. Default: BindAddr.
	MemberlistBindAddr string
	// MemberlistBindPort is the gossip port. 0 means ephemeral.
	// Default: 3322.
	MemberlistBindPort int
	// Peers is the list of host:port memberlist endpoints to join. Empty
	// means start as a cluster of one.
	Peers []string
	// LogLevel is one of "DEBUG", "INFO", "WARN", "ERROR". Default: "WARN".
	LogLevel string
	// StartTimeout bounds how long New blocks waiting for the node to
	// become ready. Default: 30s.
	StartTimeout time.Duration
}

// Store is a cache.Store backed by an embedded Olric node.
type Store struct {
	db        *olricpkg.Olric
	client    *olricpkg.EmbeddedClient
	buckets   olricpkg.DMap
	slots     olricpkg.DMap
	kv        olricpkg.DMap
	closeOnce sync.Once
}

// New starts an embedded Olric node and returns a Store wrapping its
// EmbeddedClient. Caller must call Close to leave the cluster cleanly.
func New(ctx context.Context, c Config) (*Store, error) {
	cfg := olriccfg.New("local")
	cfg.LogLevel = orDefault(c.LogLevel, "WARN")
	cfg.LogVerbosity = 1
	cfg.BindAddr = orDefault(c.BindAddr, "127.0.0.1")
	if c.BindPort != 0 {
		cfg.BindPort = c.BindPort
	}
	memberlistAddr := orDefault(c.MemberlistBindAddr, cfg.BindAddr)
	cfg.MemberlistConfig.BindAddr = memberlistAddr
	if c.MemberlistBindPort != 0 {
		cfg.MemberlistConfig.BindPort = c.MemberlistBindPort
	}
	cfg.Peers = c.Peers

	started := make(chan struct{})
	cfg.Started = func() { close(started) }

	db, err := olricpkg.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("olric: new: %w", err)
	}

	startErr := make(chan error, 1)
	go func() {
		if err := db.Start(); err != nil {
			startErr <- err
		}
	}()

	timeout := c.StartTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	select {
	case <-started:
	case err := <-startErr:
		return nil, fmt.Errorf("olric: start: %w", err)
	case <-time.After(timeout):
		_ = db.Shutdown(ctx)
		return nil, fmt.Errorf("olric: start: timeout after %s", timeout)
	case <-ctx.Done():
		_ = db.Shutdown(context.Background())
		return nil, ctx.Err()
	}

	client := db.NewEmbeddedClient()

	buckets, err := client.NewDMap("ratelimit")
	if err != nil {
		_ = db.Shutdown(ctx)
		return nil, fmt.Errorf("olric: dmap ratelimit: %w", err)
	}
	slots, err := client.NewDMap("slots")
	if err != nil {
		_ = db.Shutdown(ctx)
		return nil, fmt.Errorf("olric: dmap slots: %w", err)
	}
	kv, err := client.NewDMap("kv")
	if err != nil {
		_ = db.Shutdown(ctx)
		return nil, fmt.Errorf("olric: dmap kv: %w", err)
	}

	return &Store{
		db:      db,
		client:  client,
		buckets: buckets,
		slots:   slots,
		kv:      kv,
	}, nil
}

// TokenBucket implements cache.Store.
//
// State is stored as a 16-byte payload: float64 tokens || int64 last_unix_nanos.
// Read-modify-write is non-atomic across processes — the cluster-wide window
// allows transient overcounts under heavy contention on the same key, which
// is acceptable for rate limiting (the prior Valkey path failed open).
func (s *Store) TokenBucket(ctx context.Context, key string, capacity, refillPerSec, cost float64) (bool, time.Duration, error) {
	now := time.Now()

	tokens := capacity
	last := now

	resp, err := s.buckets.Get(ctx, key)
	switch {
	case errors.Is(err, olricpkg.ErrKeyNotFound):
		// Cold key: start at full capacity.
	case err != nil:
		return false, 0, fmt.Errorf("olric: tokenbucket get: %w", err)
	default:
		raw, gerr := resp.Byte()
		if gerr != nil {
			return false, 0, fmt.Errorf("olric: tokenbucket decode: %w", gerr)
		}
		if t, l, ok := decodeBucket(raw); ok {
			tokens = t
			last = l
		}
	}

	elapsed := now.Sub(last).Seconds()
	if elapsed > 0 {
		tokens = math.Min(capacity, tokens+elapsed*refillPerSec)
	}

	if tokens < cost {
		retry := time.Duration((cost - tokens) / refillPerSec * float64(time.Second))
		// Persist the recomputed-but-not-debited state so the next call
		// sees the correct refill basis.
		if err := s.buckets.Put(ctx, key, encodeBucket(tokens, now), olricpkg.EX(bucketTTL(capacity, refillPerSec))); err != nil {
			return false, 0, fmt.Errorf("olric: tokenbucket put: %w", err)
		}
		return false, retry, nil
	}
	tokens -= cost
	if err := s.buckets.Put(ctx, key, encodeBucket(tokens, now), olricpkg.EX(bucketTTL(capacity, refillPerSec))); err != nil {
		return false, 0, fmt.Errorf("olric: tokenbucket put: %w", err)
	}
	return true, 0, nil
}

// AcquireSlot implements cache.Store.
//
// Increments via DM.Incr (atomic per partition), self-heals if a prior
// over-Release left the counter non-positive, then either keeps the new
// value or rolls back with DM.Decr if the limit is exceeded.
func (s *Store) AcquireSlot(ctx context.Context, key string, limit int64, ttl time.Duration) (bool, int64, error) {
	cur, err := s.slots.Incr(ctx, key, 1)
	if err != nil {
		return false, 0, fmt.Errorf("olric: acquire incr: %w", err)
	}

	if cur <= 0 {
		// Self-heal: a previous over-Release pushed the counter to or
		// below zero. Reset to 1 (this Acquire counts as the first
		// active slot) and continue.
		if err := s.slots.Put(ctx, key, 1, olricpkg.EX(ttl)); err != nil {
			return false, 0, fmt.Errorf("olric: acquire heal: %w", err)
		}
		return true, 1, nil
	}

	if int64(cur) > limit {
		if _, derr := s.slots.Decr(ctx, key, 1); derr != nil {
			return false, 0, fmt.Errorf("olric: acquire rollback: %w", derr)
		}
		return false, limit, nil
	}

	if err := s.slots.Expire(ctx, key, ttl); err != nil && !errors.Is(err, olricpkg.ErrKeyNotFound) {
		return false, 0, fmt.Errorf("olric: acquire expire: %w", err)
	}
	return true, int64(cur), nil
}

// ReleaseSlot implements cache.Store. Reads-then-decrements; clamps at zero
// to avoid going negative on spurious extra releases. Race window is bounded
// by the per-key partition owner — see AcquireSlot's self-heal.
func (s *Store) ReleaseSlot(ctx context.Context, key string) error {
	resp, err := s.slots.Get(ctx, key)
	if errors.Is(err, olricpkg.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("olric: release get: %w", err)
	}
	cur, err := resp.Int()
	if err != nil {
		return fmt.Errorf("olric: release decode: %w", err)
	}
	if cur <= 0 {
		return nil
	}
	if _, err := s.slots.Decr(ctx, key, 1); err != nil {
		return fmt.Errorf("olric: release decr: %w", err)
	}
	return nil
}

// RefreshSlot implements cache.Store.
func (s *Store) RefreshSlot(ctx context.Context, key string, ttl time.Duration) error {
	if err := s.slots.Expire(ctx, key, ttl); err != nil && !errors.Is(err, olricpkg.ErrKeyNotFound) {
		return fmt.Errorf("olric: refresh: %w", err)
	}
	return nil
}

// Get implements cache.Store.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.kv.Get(ctx, key)
	if errors.Is(err, olricpkg.ErrKeyNotFound) {
		return nil, cache.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("olric: get: %w", err)
	}
	v, err := resp.Byte()
	if err != nil {
		return nil, fmt.Errorf("olric: get decode: %w", err)
	}
	return v, nil
}

// Set implements cache.Store.
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	var opts []olricpkg.PutOption
	if ttl > 0 {
		opts = append(opts, olricpkg.EX(ttl))
	}
	if err := s.kv.Put(ctx, key, value, opts...); err != nil {
		return fmt.Errorf("olric: set: %w", err)
	}
	return nil
}

// Delete implements cache.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.kv.Delete(ctx, key); err != nil {
		return fmt.Errorf("olric: delete: %w", err)
	}
	return nil
}

// Close implements cache.Store.
func (s *Store) Close(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		err = s.db.Shutdown(ctx)
	})
	return err
}

func encodeBucket(tokens float64, last time.Time) []byte {
	out := make([]byte, 16)
	binary.LittleEndian.PutUint64(out[0:8], math.Float64bits(tokens))
	// last.UnixNano() is int64; reinterpret as uint64 bits without sign loss.
	//nolint:gosec // bit-level reinterpretation, not a numeric conversion.
	binary.LittleEndian.PutUint64(out[8:16], uint64(last.UnixNano()))
	return out
}

func decodeBucket(raw []byte) (float64, time.Time, bool) {
	if len(raw) != 16 {
		return 0, time.Time{}, false
	}
	tokens := math.Float64frombits(binary.LittleEndian.Uint64(raw[0:8]))
	//nolint:gosec // bit-level reinterpretation back to int64; the value
	// originated as int64 in encodeBucket.
	last := time.Unix(0, int64(binary.LittleEndian.Uint64(raw[8:16])))
	return tokens, last, true
}

// bucketTTL keeps a bucket alive long enough to refill twice from empty,
// with a 60-second floor so rarely-used keys don't thrash.
func bucketTTL(capacity, refillPerSec float64) time.Duration {
	if refillPerSec <= 0 {
		return 60 * time.Second
	}
	full := time.Duration(capacity/refillPerSec*float64(time.Second)) * 2
	if full < 60*time.Second {
		return 60 * time.Second
	}
	return full
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
