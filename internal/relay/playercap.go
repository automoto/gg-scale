package relay

import (
	"encoding/binary"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"
)

// playerAllocLimiter throttles authenticated TURN allocations per player so one
// credential cannot flood the node with allocations and monopolise the global
// pool. The budget is sized well above a real client's tiny operation rate but
// far below an allocation flood.
//
// State is a fixed array of token-bucket cells indexed by a hash of the
// credential subject. It is allocated once, never grows, needs no eviction,
// and has no full condition. Two subjects can share a cell and then share a
// budget, which is acceptable because the budget sits far above a real
// client's operation rate. A per-instance random seed makes those incidental
// collisions unpredictable.
//
// The relay node holds no database, so this state is process-local (per relay
// VM) — the correct scope, since each VM defends its own port pool.
type playerAllocLimiter struct {
	mu    sync.Mutex
	cells []allocCell

	// rate is tokens per second; burst is the cell ceiling.
	rate  float64
	burst float64

	seed      maphash.Seed
	now       func() time.Time
	throttled atomic.Int64
}

// allocCells is the fixed number of token-bucket cells. At 32 bytes per cell
// this is 2 MiB per relay VM, allocated once at startup. Sized well above the
// player count a single relay node serves, so sharing is rare in practice.
const allocCells = 1 << 16

// allocCell is one token bucket. A zero cell self-initialises on first use:
// the elapsed time since the zero instant is enormous, so the refill below
// saturates it to burst.
type allocCell struct {
	tokens float64
	last   time.Time
}

// newPlayerAllocLimiter returns a limiter allowing perMinute authenticated
// allocations per player with the given burst. perMinute <= 0 or burst <= 0
// returns nil, which allow() treats as unlimited.
func newPlayerAllocLimiter(perMinute, burst int) *playerAllocLimiter {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	return &playerAllocLimiter{
		cells: make([]allocCell, allocCells),
		rate:  float64(perMinute) / 60.0,
		burst: float64(burst),
		seed:  maphash.MakeSeed(),
		now:   time.Now,
	}
}

// Throttled reports the cumulative count of allocations refused by the
// per-player budget.
func (p *playerAllocLimiter) Throttled() int64 {
	if p == nil {
		return 0
	}
	return p.throttled.Load()
}

// allow reports whether the player may create one more TURN allocation now. A
// nil limiter is unlimited.
func (p *playerAllocLimiter) allow(tenantID, playerID int64) bool {
	if p == nil {
		return true
	}
	now := p.now()
	idx := p.cellIndex(tenantID, playerID)

	p.mu.Lock()
	defer p.mu.Unlock()
	c := &p.cells[idx]
	if elapsed := now.Sub(c.last); elapsed > 0 {
		c.tokens = min(p.burst, c.tokens+elapsed.Seconds()*p.rate)
		c.last = now
	}
	if c.tokens < 1 {
		p.throttled.Add(1)
		return false
	}
	c.tokens--
	return true
}

// cellIndex maps a credential subject to its token-bucket cell.
func (p *playerAllocLimiter) cellIndex(tenantID, playerID int64) uint64 {
	var buf [16]byte
	// Reinterpreting the ids as unsigned is the point: the hash wants their bit
	// pattern, and the conversion is bijective, so no value is lost.
	binary.LittleEndian.PutUint64(buf[0:8], uint64(tenantID))  //nolint:gosec // hashing a bit pattern, not a magnitude
	binary.LittleEndian.PutUint64(buf[8:16], uint64(playerID)) //nolint:gosec // hashing a bit pattern, not a magnitude
	return maphash.Bytes(p.seed, buf[:]) % uint64(len(p.cells))
}
