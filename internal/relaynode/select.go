package relaynode

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrNoEligibleNodes is returned when no node can be advertised.
var ErrNoEligibleNodes = errors.New("relaynode: no eligible relay nodes")

// cacheTTL is how long a Selector reuses one read of the eligible nodes. The
// set changes at most once per ProbeInterval, so a short reuse saves a
// database round trip per credential request at little cost in freshness.
const cacheTTL = 2 * time.Second

// EligibleFunc lists the nodes relay credentials may advertise.
type EligibleFunc func(ctx context.Context) ([]Node, error)

// Selection is the relay URLs one credential set advertises.
type Selection struct {
	URLs []string
	// Region is the region served, or "" when the nodes span regions.
	Region string
}

// Selector picks the relay URLs for a credential request. Safe for
// concurrent use.
type Selector struct {
	eligible EligibleFunc
	embedded *Node
	now      func() time.Time

	mu       sync.Mutex
	cached   []Node
	cachedAt time.Time
}

// NewSelector returns a Selector over eligible. embedded, when non-nil, is the
// relay running inside this API server: it has no region, it is always
// eligible while the process runs, and it is served only in the cross-region
// fallback.
func NewSelector(eligible EligibleFunc, embedded *Node) *Selector {
	return &Selector{eligible: eligible, embedded: embedded, now: time.Now}
}

// Select returns the URLs of every eligible node in region, in shuffled node
// order. region is matched case-insensitively. With no eligible node in
// region, or no region, it falls back to every eligible node. Each node's URLs
// keep their registered order, so a client that tries them in order still
// prefers that node's first transport. When the eligible nodes cannot be read,
// the embedded relay is served alone if there is one.
func (s *Selector) Select(ctx context.Context, region string) (Selection, error) {
	nodes, err := s.nodes(ctx)
	if err != nil && s.embedded == nil {
		return Selection{}, err
	}
	region = strings.ToLower(strings.TrimSpace(region))
	var candidates []Node
	if region != "" {
		for _, n := range nodes {
			if n.Region == region {
				candidates = append(candidates, n)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = slices.Clone(nodes)
		if s.embedded != nil {
			candidates = append(candidates, *s.embedded)
		}
	}
	if len(candidates) == 0 {
		return Selection{}, ErrNoEligibleNodes
	}
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	sel := Selection{Region: candidates[0].Region}
	for _, n := range candidates {
		if n.Region != sel.Region {
			sel.Region = ""
		}
		sel.URLs = append(sel.URLs, n.TURNURLs...)
	}
	return sel, nil
}

// nodes returns the eligible nodes, reusing a read younger than cacheTTL. The
// returned slice is shared and must not be modified.
func (s *Selector) nodes(ctx context.Context) ([]Node, error) {
	now := s.now()
	s.mu.Lock()
	if !s.cachedAt.IsZero() && now.Sub(s.cachedAt) < cacheTTL {
		nodes := s.cached
		s.mu.Unlock()
		return nodes, nil
	}
	s.mu.Unlock()
	nodes, err := s.eligible(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = nodes, now
	s.mu.Unlock()
	return nodes, nil
}
