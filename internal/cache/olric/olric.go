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
	"strings"
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
	db         *olricpkg.Olric
	client     *olricpkg.EmbeddedClient
	buckets    olricpkg.DMap
	slots      olricpkg.DMap
	burstSlots olricpkg.DMap
	burstLocks olricpkg.DMap
	kv         olricpkg.DMap
	closeOnce  sync.Once
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
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: start: timeout after %s", timeout)
	case <-ctx.Done():
		shutdownOlric(db)
		return nil, ctx.Err()
	}

	client := db.NewEmbeddedClient()

	buckets, err := client.NewDMap("ratelimit")
	if err != nil {
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: dmap ratelimit: %w", err)
	}
	slots, err := client.NewDMap("slots")
	if err != nil {
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: dmap slots: %w", err)
	}
	burstSlots, err := client.NewDMap("burst_slots")
	if err != nil {
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: dmap burst_slots: %w", err)
	}
	burstLocks, err := client.NewDMap("burst_slot_locks")
	if err != nil {
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: dmap burst_slot_locks: %w", err)
	}
	kv, err := client.NewDMap("kv")
	if err != nil {
		shutdownOlric(db)
		return nil, fmt.Errorf("olric: dmap kv: %w", err)
	}

	return &Store{
		db:         db,
		client:     client,
		buckets:    buckets,
		slots:      slots,
		burstSlots: burstSlots,
		burstLocks: burstLocks,
		kv:         kv,
	}, nil
}

// shutdownOlric runs db.Shutdown against a fresh background context with a
// short timeout. The startup context may have been cancelled (the caller's
// New() can be invoked under a request ctx), so reusing it on the error
// path can strand goroutines and sockets.
func shutdownOlric(db olricShutdowner) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = db.Shutdown(ctx)
}

// olricShutdowner is the slice of *olric.Olric shutdownOlric uses, kept as
// an interface so the helper is testable without spinning a real cluster.
type olricShutdowner interface {
	Shutdown(context.Context) error
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
	// min(capacity, ...) caps a refund (negative cost) so a credited token can
	// never lift the bucket above its burst; a no-op for the normal cost>=0 path.
	tokens = math.Min(capacity, tokens-cost)
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
		if err := s.slots.Expire(ctx, key, ttl); err != nil && !errors.Is(err, olricpkg.ErrKeyNotFound) {
			return false, 0, fmt.Errorf("olric: acquire reject expire: %w", err)
		}
		return false, limit, nil
	}

	if err := s.slots.Expire(ctx, key, ttl); err != nil && !errors.Is(err, olricpkg.ErrKeyNotFound) {
		return false, 0, fmt.Errorf("olric: acquire expire: %w", err)
	}
	return true, int64(cur), nil
}

// AcquireSlotBurst implements cache.Store as an atomic per-key transition of
// the shared burst model. The dedicated distributed lock spans the state Get
// and Put so concurrent callers cannot overwrite one another's increments.
func (s *Store) AcquireSlotBurst(ctx context.Context, key string, sustained, ceiling int64, burstBudget, ttl time.Duration) (bool, int64, error) {
	var acquired bool
	var current int64
	err := s.withBurstLock(ctx, key, func() error {
		st, err := s.loadBurst(ctx, key)
		if err != nil {
			return err
		}
		now := time.Now()
		if !st.Expires.IsZero() && st.Expires.Before(now) {
			st = cache.BurstSlotState{}
		}
		acquired = cache.AdmitBurst(&st, sustained, ceiling, burstBudget, ttl, now)
		current = st.Count
		if err := s.burstSlots.Put(ctx, key, encodeBurst(st), olricpkg.EX(ttl)); err != nil {
			return fmt.Errorf("olric: burst put: %w", err)
		}
		return nil
	})
	return acquired, current, err
}

const (
	burstLockLease    = 15 * time.Second
	burstLockDeadline = 5 * time.Second
	burstUnlockLimit  = 2 * time.Second
)

// withBurstLock serializes every read-modify-write of one distributed burst
// slot. DMap commands are individually atomic, but the state transition spans
// Get and Put and therefore needs an explicit per-key lock.
func (s *Store) withBurstLock(ctx context.Context, key string, fn func() error) (err error) {
	lock, err := s.burstLocks.LockWithTimeout(ctx, key, burstLockLease, burstLockDeadline)
	if err != nil {
		return fmt.Errorf("olric: burst lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), burstUnlockLimit)
		defer cancel()
		if unlockErr := lock.Unlock(unlockCtx); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("olric: burst unlock: %w", unlockErr))
		}
	}()
	return fn()
}

// loadBurst reads and decodes the burst state at key, returning a zero state
// when absent or undecodable (self-heal).
func (s *Store) loadBurst(ctx context.Context, key string) (cache.BurstSlotState, error) {
	resp, err := s.burstSlots.Get(ctx, key)
	if errors.Is(err, olricpkg.ErrKeyNotFound) {
		return cache.BurstSlotState{}, nil
	}
	if err != nil {
		return cache.BurstSlotState{}, fmt.Errorf("olric: burst get: %w", err)
	}
	raw, err := resp.Byte()
	if err != nil {
		return cache.BurstSlotState{}, fmt.Errorf("olric: burst decode: %w", err)
	}
	st, ok := decodeBurst(raw)
	if !ok {
		return cache.BurstSlotState{}, nil
	}
	return st, nil
}

// releaseBurst decrements a burst slot's count, clamped at zero. A no-op when
// no burst slot exists at key.
func (s *Store) releaseBurstLocked(ctx context.Context, key string) error {
	st, err := s.loadBurst(ctx, key)
	if err != nil {
		return err
	}
	if st.Count <= 0 {
		return nil
	}
	now := time.Now()
	ttl := st.Expires.Sub(now)
	if ttl <= 0 {
		if _, derr := s.burstSlots.Delete(ctx, key); derr != nil && !errors.Is(derr, olricpkg.ErrKeyNotFound) {
			return fmt.Errorf("olric: burst release delete: %w", derr)
		}
		return nil
	}
	cache.ReleaseBurst(&st, now)
	if err := s.burstSlots.Put(ctx, key, encodeBurst(st), olricpkg.EX(ttl)); err != nil {
		return fmt.Errorf("olric: burst release put: %w", err)
	}
	return nil
}

// ReleaseSlot implements cache.Store. Plain counters only. Reads-then-
// decrements; clamps at zero to avoid going negative on spurious extra
// releases. Race window is bounded by the per-key partition owner — see
// AcquireSlot's self-heal.
func (s *Store) ReleaseSlot(ctx context.Context, key string) error {
	resp, err := s.slots.Get(ctx, key)
	switch {
	case errors.Is(err, olricpkg.ErrKeyNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("olric: release get: %w", err)
	default:
		cur, derr := resp.Int()
		if derr != nil {
			return fmt.Errorf("olric: release decode: %w", derr)
		}
		if cur > 0 {
			if _, derr := s.slots.Decr(ctx, key, 1); derr != nil {
				return fmt.Errorf("olric: release decr: %w", derr)
			}
		}
	}
	return nil
}

// ReleaseSlotBurst implements cache.Store. Burst counters only.
func (s *Store) ReleaseSlotBurst(ctx context.Context, key string) error {
	return s.withBurstLock(ctx, key, func() error {
		return s.releaseBurstLocked(ctx, key)
	})
}

// RefreshSlot implements cache.Store. Extends a plain counter's idle TTL; the
// absent-key not-found is expected and ignored.
func (s *Store) RefreshSlot(ctx context.Context, key string, ttl time.Duration) error {
	if err := s.slots.Expire(ctx, key, ttl); !isKeyNotFound(err) {
		return fmt.Errorf("olric: refresh: %w", err)
	}
	return nil
}

// RefreshSlotBurst implements cache.Store. Extends a live burst slot's idle
// TTL. A no-op on an absent, expired, or already-released (Count<=0) slot so a
// stray refresh cannot revive a counter about to be reaped.
func (s *Store) RefreshSlotBurst(ctx context.Context, key string, ttl time.Duration) error {
	return s.withBurstLock(ctx, key, func() error {
		st, err := s.loadBurst(ctx, key)
		if err != nil {
			return err
		}
		now := time.Now()
		if st.Count <= 0 || (!st.Expires.IsZero() && st.Expires.Before(now)) {
			return nil
		}
		cache.RefreshBurst(&st, ttl, now)
		if err := s.burstSlots.Put(ctx, key, encodeBurst(st), olricpkg.EX(ttl)); err != nil {
			return fmt.Errorf("olric: refresh burst put: %w", err)
		}
		return nil
	})
}

// isKeyNotFound reports whether err is nil or an Olric key-not-found. DMap.Expire
// surfaces a not-found that errors.Is doesn't match against the top-level
// sentinel, so fall back to the message.
func isKeyNotFound(err error) bool {
	return err == nil || errors.Is(err, olricpkg.ErrKeyNotFound) ||
		strings.Contains(err.Error(), "key not found")
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
	return append([]byte(nil), v...), nil
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
	for name, dmap := range map[string]olricpkg.DMap{
		"ratelimit":   s.buckets,
		"slots":       s.slots,
		"burst_slots": s.burstSlots,
		"kv":          s.kv,
	} {
		if _, err := dmap.Delete(ctx, key); err != nil && !errors.Is(err, olricpkg.ErrKeyNotFound) {
			return fmt.Errorf("olric: delete %s: %w", name, err)
		}
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

// encodeBurst serialises a burst slot as six little-endian int64s: count,
// remaining budget (ns), last-assessed (UnixNano), expires (UnixNano),
// sustained count, and configured budget (ns). Fixed width keeps state
// identical across nodes; decodeBurst also accepts the previous four-field
// shape so a rolling deploy self-heals on the next acquire.
func encodeBurst(st cache.BurstSlotState) []byte {
	out := make([]byte, 48)
	putInt64(out[0:8], st.Count)
	putInt64(out[8:16], int64(st.BurstRemaining))
	putInt64(out[16:24], unixNanoOrZero(st.LastAssessed))
	putInt64(out[24:32], unixNanoOrZero(st.Expires))
	putInt64(out[32:40], st.Sustained)
	putInt64(out[40:48], int64(st.BurstBudget))
	return out
}

func decodeBurst(raw []byte) (cache.BurstSlotState, bool) {
	if len(raw) != 32 && len(raw) != 48 {
		return cache.BurstSlotState{}, false
	}
	st := cache.BurstSlotState{
		Count:          readInt64(raw[0:8]),
		BurstRemaining: time.Duration(readInt64(raw[8:16])),
		LastAssessed:   timeFromUnixNano(readInt64(raw[16:24])),
		Expires:        timeFromUnixNano(readInt64(raw[24:32])),
	}
	if len(raw) == 48 {
		st.Sustained = readInt64(raw[32:40])
		st.BurstBudget = time.Duration(readInt64(raw[40:48]))
	}
	return st, true
}

func putInt64(dst []byte, value int64) {
	//nolint:gosec // Preserve the signed value's bit pattern in an unsigned container.
	binary.LittleEndian.PutUint64(dst, uint64(value))
}

func readInt64(src []byte) int64 {
	//nolint:gosec // Restore the signed bit pattern written by putInt64.
	return int64(binary.LittleEndian.Uint64(src))
}

func unixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
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
