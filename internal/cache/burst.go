package cache

import "time"

// BurstRefillWindow is how long a slot must sit at/below its sustained cap to
// refill the full burst budget. Draining above sustained is proportional to how
// far above (full-2× load drains 1:1); refilling is linear over this window.
const BurstRefillWindow = time.Hour

// BurstSlotState is the serialized state behind AcquireSlotBurst: the live
// connection count, the remaining burst budget, and when the budget was last
// assessed. Shared verbatim by the memory and olric backends so the state has
// exactly one shape and one set of semantics.
type BurstSlotState struct {
	Count          int64
	BurstRemaining time.Duration
	LastAssessed   time.Time
	Expires        time.Time
	Sustained      int64
	BurstBudget    time.Duration
}

// AdmitBurst assesses st against the elapsed time to now, then decides whether
// one more connection is admitted under the sustained/ceiling burst model:
//
//   - Connections up to sustained are always admitted (the sustained cap).
//   - Between sustained and ceiling (== 2× sustained) a connection is admitted
//     only while burst budget remains.
//   - ceiling is a hard wall: never admit past it.
//
// While the current count is above sustained the budget drains by
// elapsed × (count−sustained)/sustained (so camping at 2× drains 1:1 and burns
// a full budget in burstBudget of wall time). At/below sustained it refills at
// burstBudget/BurstRefillWindow up to burstBudget. st is mutated in place; on
// admission Count and Expires advance. The bool reports admission — the caller
// infers the rejection reason by comparing st.Count to ceiling.
func AdmitBurst(st *BurstSlotState, sustained, ceiling int64, burstBudget, ttl time.Duration, now time.Time) bool {
	if st.LastAssessed.IsZero() {
		st.BurstRemaining = burstBudget
		st.LastAssessed = now
	}
	st.Sustained = sustained
	st.BurstBudget = burstBudget
	if st.BurstRemaining > burstBudget {
		st.BurstRemaining = burstBudget
	}
	AssessBurst(st, now)

	newCount := st.Count + 1
	switch {
	case newCount > ceiling:
		return false
	case newCount > sustained && st.BurstRemaining <= 0:
		return false
	}
	st.Count = newCount
	st.Expires = now.Add(ttl)
	return true
}

// AssessBurst charges or refills the elapsed interval at the current live
// count. The model parameters are persisted with the slot so releases and
// heartbeats can account for time without their callers repeating them.
func AssessBurst(st *BurstSlotState, now time.Time) {
	if st.LastAssessed.IsZero() || st.BurstBudget <= 0 {
		return
	}
	elapsed := now.Sub(st.LastAssessed)
	if elapsed <= 0 {
		return
	}
	if st.Sustained > 0 && st.Count > st.Sustained {
		drain := time.Duration(float64(elapsed) * float64(st.Count-st.Sustained) / float64(st.Sustained))
		st.BurstRemaining = max(0, st.BurstRemaining-drain)
	} else {
		refill := time.Duration(float64(elapsed) * float64(st.BurstBudget) / float64(BurstRefillWindow))
		st.BurstRemaining = min(st.BurstBudget, st.BurstRemaining+refill)
	}
	st.LastAssessed = now
}

// ReleaseBurst charges the time spent at the old count before releasing one
// connection. This prevents disconnecting burst holders from erasing the
// budget they consumed.
func ReleaseBurst(st *BurstSlotState, now time.Time) {
	AssessBurst(st, now)
	if st.Count > 0 {
		st.Count--
	}
}

// RefreshBurst charges elapsed time before extending the slot's idle TTL.
func RefreshBurst(st *BurstSlotState, ttl time.Duration, now time.Time) {
	AssessBurst(st, now)
	st.Expires = now.Add(ttl)
}
