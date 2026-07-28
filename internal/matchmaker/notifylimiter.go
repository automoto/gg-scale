package matchmaker

import (
	"sync"
	"time"
)

// notifyLimiterPruneThreshold is the map size past which allow() drops keys
// whose last send is older than the debounce interval. Keeps the map bounded
// to roughly the set of buckets active within one interval.
const notifyLimiterPruneThreshold = 1024

// notifyLimiter coalesces matchmaker wakeups: at most one NOTIFY per key
// (the enqueuing player) per interval. The worker tolerates lost
// notifications via its fallback ticker, so debouncing an extra enqueue only
// defers its wakeup by at most one interval. Mirrors River's client-side
// notify debounce.
type notifyLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
	now      func() time.Time
}

func newNotifyLimiter(interval time.Duration) *notifyLimiter {
	return &notifyLimiter{
		interval: interval,
		last:     make(map[string]time.Time),
		now:      time.Now,
	}
}

// allow reports whether a NOTIFY for key should be sent now, recording the
// send time when it returns true.
func (l *notifyLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if last, ok := l.last[key]; ok && now.Sub(last) < l.interval {
		return false
	}
	l.prune(now)
	l.last[key] = now
	return true
}

// forget releases key's debounce reservation so the next allow succeeds
// immediately. Called when the NOTIFY that reserved the window failed to
// send, so a failed send doesn't consume the window. A concurrent allow for
// the same key between the failed send and the forget would be rolled back
// too, but a key covers a single player's enqueues and the one-active-ticket
// constraint keeps those sequential in practice.
func (l *notifyLimiter) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, key)
}

// prune drops stale keys once the map grows past the threshold. Called under
// l.mu.
func (l *notifyLimiter) prune(now time.Time) {
	if len(l.last) < notifyLimiterPruneThreshold {
		return
	}
	for k, t := range l.last {
		if now.Sub(t) >= l.interval {
			delete(l.last, k)
		}
	}
}

// size returns the number of tracked keys (test helper).
func (l *notifyLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.last)
}
