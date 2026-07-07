package matchmaker

import (
	"slices"
	"time"

	"github.com/ggscale/ggscale/internal/matchmaker/query"
)

// groupConfig tunes group formation for one bucket pass.
type groupConfig struct {
	// relaxAfter is how long the oldest member of a below-max group must
	// have waited before the group commits at a smaller (still valid)
	// size. Groups at their joint max commit immediately.
	relaxAfter time.Duration
	// regionRelaxAfter is how long the bucket's oldest widen-eligible
	// ticket must have waited before cross-region grouping unlocks for
	// allow_cross_region tickets. 0 disables widening entirely.
	regionRelaxAfter time.Duration
	// queries maps ticket id → parsed criteria. A missing/nil entry
	// matches everything. Membership is mutual: every member's query must
	// accept every other member's properties.
	queries map[int64]query.Expr
	// props memoises each ticket's property view so acceptance checks don't
	// rebuild the same map on every pairing (formGroups is O(n^2) in
	// candidates). Populated once per bucket pass; a missing entry falls
	// back to computing on the fly.
	props map[int64]query.Props
	// onQueryReject fires once per candidate pairing rejected by mutual
	// acceptance (metrics hook). May be nil.
	onQueryReject func()
}

// accepts reports whether a's query accepts b's properties.
func (cfg groupConfig) accepts(a, b *Ticket) bool {
	e := cfg.queries[a.ID]
	if e == nil {
		return true
	}
	return e.Eval(cfg.propsFor(b))
}

// propsFor returns b's memoised property view, computing it if the cache
// wasn't pre-populated (e.g. a direct caller in tests).
func (cfg groupConfig) propsFor(b *Ticket) query.Props {
	if p, ok := cfg.props[b.ID]; ok {
		return p
	}
	return ticketProps(b)
}

// mutualAccept applies bidirectional query acceptance to one pair.
func (cfg groupConfig) mutualAccept(a, b *Ticket) bool {
	if cfg.accepts(a, b) && cfg.accepts(b, a) {
		return true
	}
	if cfg.onQueryReject != nil {
		cfg.onQueryReject()
	}
	return false
}

// ticketProps is the property view queries evaluate against: the ticket's
// string/numeric properties plus the implicit read-only region and
// game_mode keys.
func ticketProps(t *Ticket) query.Props {
	strs := make(map[string]string, len(t.StringProperties)+2)
	for k, v := range t.StringProperties {
		strs[k] = v
	}
	strs["region"] = t.Region
	strs["game_mode"] = t.GameMode
	return query.Props{Strings: strs, Numbers: t.NumericProperties}
}

// formGroups partitions claimed tickets into committable rosters.
//
// Greedy, oldest-first: each unused ticket seeds a group and pulls in
// compatible candidates (same-region first) until the group's joint max is
// reached. A group commits when it is full, or — once the seed has waited
// past relaxAfter — at the largest size every member's
// min/max/count_multiple accepts. Tickets that end up in no group stay
// claimed and are returned to the queue by the caller.
//
// Region rules (non-fleet buckets mix regions): members must share a region
// or have none, until the bucket widens — after the oldest
// allow_cross_region ticket has waited past regionRelaxAfter, widen-eligible
// tickets group across regions. Same-region candidates are still preferred
// after widening.
func formGroups(tickets []*Ticket, now time.Time, cfg groupConfig) [][]*Ticket {
	if cfg.props == nil {
		cfg.props = make(map[int64]query.Props, len(tickets))
		for _, t := range tickets {
			cfg.props[t.ID] = ticketProps(t)
		}
	}
	pool := slices.Clone(tickets)
	slices.SortFunc(pool, func(a, b *Ticket) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})

	widened := bucketWidened(pool, now, cfg.regionRelaxAfter)
	used := make(map[int64]bool, len(pool))
	var groups [][]*Ticket

	for _, seed := range pool {
		if used[seed.ID] {
			continue
		}
		group := fillGroup(seed, pool, used, widened, cfg)
		size := largestValidSize(group)
		if size == 0 {
			continue
		}
		full := size == jointMax(group[:size])
		relaxed := now.Sub(seed.CreatedAt) >= cfg.relaxAfter
		if !full && !relaxed {
			continue
		}
		group = group[:size]
		for _, t := range group {
			used[t.ID] = true
		}
		groups = append(groups, group)
	}
	return groups
}

// bucketWidened reports whether cross-region grouping has unlocked: the
// oldest widen-eligible (allow_cross_region, region-pinned) ticket has
// waited past the window.
func bucketWidened(pool []*Ticket, now time.Time, window time.Duration) bool {
	if window <= 0 {
		return false
	}
	for _, t := range pool {
		if t.AllowCrossRegion && t.Region != "" && now.Sub(t.CreatedAt) >= window {
			return true
		}
	}
	return false
}

// fillGroup pulls candidates into seed's group, same-region first, without
// exceeding any member's max_count.
func fillGroup(seed *Ticket, pool []*Ticket, used map[int64]bool, widened bool, cfg groupConfig) []*Ticket {
	group := []*Ticket{seed}
	sameRegionFirst := func(preferSame bool) {
		for _, c := range pool {
			if used[c.ID] || c.ID == seed.ID || slices.Contains(group, c) {
				continue
			}
			if preferSame != (c.Region == seed.Region) {
				continue
			}
			if len(group)+1 > min(jointMax(group), c.MaxCount) {
				continue
			}
			if !compatibleWithAll(group, c, widened, cfg) {
				continue
			}
			group = append(group, c)
			if len(group) == jointMax(group) {
				return
			}
		}
	}
	sameRegionFirst(true)
	if len(group) < jointMax(group) {
		sameRegionFirst(false)
	}
	return group
}

// compatibleWithAll reports whether candidate c may join every member of
// group under the region and mutual-query rules.
func compatibleWithAll(group []*Ticket, c *Ticket, widened bool, cfg groupConfig) bool {
	for _, m := range group {
		if !regionsCompatible(m, c, widened) {
			return false
		}
		if !cfg.mutualAccept(m, c) {
			return false
		}
	}
	return true
}

// regionsCompatible applies the soft-region rule to one pair.
func regionsCompatible(a, b *Ticket, widened bool) bool {
	if a.Region == b.Region || a.Region == "" || b.Region == "" {
		return true
	}
	return widened && a.AllowCrossRegion && b.AllowCrossRegion
}

// jointMax is the largest size every member's max_count accepts.
func jointMax(group []*Ticket) int {
	m := group[0].MaxCount
	for _, t := range group[1:] {
		m = min(m, t.MaxCount)
	}
	return m
}

// largestValidSize trims the group (built oldest-first) to the largest
// prefix size that satisfies every kept member's min/max/count_multiple.
// Returns 0 when no prefix works.
func largestValidSize(group []*Ticket) int {
	for size := len(group); size >= 1; size-- {
		if validSize(group[:size], size) {
			return size
		}
	}
	return 0
}

func validSize(members []*Ticket, size int) bool {
	for _, t := range members {
		if size < t.MinCount || size > t.MaxCount || size%t.CountMultiple != 0 {
			return false
		}
	}
	return true
}
