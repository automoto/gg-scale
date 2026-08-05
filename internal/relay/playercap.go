package relay

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// playerKey identifies a credential subject; the username embeds both, and
// playerID is only unique within a tenant.
type playerKey struct {
	tenant int64
	player int64
}

// playerAllocLimiter throttles authenticated TURN operations per player so a
// single credential can't flood the node with allocations and monopolise the
// global pool. pion invokes the AuthHandler on every authenticated request
// (Allocate, Refresh, CreatePermission, ChannelBind), so the bucket is sized
// well above a legit client's tiny op rate but far below an allocation flood; a
// throttled request is answered 401 and no allocation is created. Combined with
// the ~10-minute allocation lifetime, this bounds one credential to roughly
// rate×lifetime concurrent allocations instead of the entire pool.
//
// The relay node holds no database, so state is process-local (per relay VM) —
// the correct scope, since each VM defends its own port pool. Buckets are held
// in a bounded map, lazily swept of players idle longer than ttl, and hard-
// capped at maxBuckets so a flood of distinct forged subjects (an authenticated
// attacker who has seen one valid key id) cannot grow the map without bound.
type playerAllocLimiter struct {
	mu         sync.Mutex
	buckets    map[playerKey]*playerBucket
	rate       rate.Limit
	burst      int
	ttl        time.Duration
	maxBuckets int
	nextSweep  time.Time
	now        func() time.Time
}

// maxPlayerBuckets bounds the limiter map. At ~100 bytes per entry this caps
// the map near 10 MB per relay VM. Normal operation stays far below it; the cap
// only bites during an active flood, where new subjects are refused tracking
// (and thus their TURN op) until the sweep frees idle entries.
const maxPlayerBuckets = 100_000

type playerBucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// newPlayerAllocLimiter returns a limiter allowing perMinute authenticated ops
// per player with the given burst. perMinute <= 0 or burst <= 0 returns nil,
// which allow() treats as unlimited.
func newPlayerAllocLimiter(perMinute, burst int) *playerAllocLimiter {
	if perMinute <= 0 || burst <= 0 {
		return nil
	}
	return &playerAllocLimiter{
		buckets:    make(map[playerKey]*playerBucket),
		rate:       rate.Limit(float64(perMinute) / 60.0),
		burst:      burst,
		ttl:        10 * time.Minute,
		maxBuckets: maxPlayerBuckets,
		now:        time.Now,
	}
}

// allow reports whether the player may perform one more authenticated op now. A
// nil limiter is unlimited.
func (p *playerAllocLimiter) allow(tenantID, playerID int64) bool {
	if p == nil {
		return true
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	key := playerKey{tenant: tenantID, player: playerID}
	b := p.buckets[key]
	if b == nil {
		// Map full even after the sweep: refuse to track a new subject rather
		// than grow without bound. The op is denied (401, no allocation); an
		// already-tracked legit player is unaffected because its bucket exists.
		if p.maxBuckets > 0 && len(p.buckets) >= p.maxBuckets {
			return false
		}
		b = &playerBucket{lim: rate.NewLimiter(p.rate, p.burst)}
		p.buckets[key] = b
	}
	b.lastSeen = now
	return b.lim.AllowN(now, 1)
}

// sweepLocked drops buckets for players idle beyond ttl, at most once per ttl so
// the hot path stays cheap. Caller holds p.mu.
func (p *playerAllocLimiter) sweepLocked(now time.Time) {
	if now.Before(p.nextSweep) {
		return
	}
	p.nextSweep = now.Add(p.ttl)
	for k, b := range p.buckets {
		if now.Sub(b.lastSeen) > p.ttl {
			delete(p.buckets, k)
		}
	}
}
