package relay

import (
	"errors"
	"sync"
	"sync/atomic"
)

var (
	errPlayerAllocationCap = errors.New("relay: player allocation cap reached")
	errTenantAllocationCap = errors.New("relay: tenant allocation cap reached")
)

// subjectAllocCaps caps the live allocations one player and one tenant may hold
// on this relay node. The rate limit in playercap.go bounds how fast a player
// allocates; this bounds how many it holds, since refreshed allocations live on.
//
// Counts go up when the relay address generator hands out a relayed socket and
// down when pion closes it. Pion closes it on every delete path: a Refresh with
// LIFETIME 0, lifetime expiry, and a relay read error. Maps hold only subjects
// with a live allocation, so they stay bounded by the node allocation cap.
//
// The tenant cap comes from the last verified credential of that tenant (see
// Issuer.IssueWithTenantCap); a credential without one gets defaultTenant.
type subjectAllocCaps struct {
	perPlayer     int64 // 0 = unlimited
	defaultTenant int64 // 0 = unlimited

	mu        sync.Mutex
	players   map[allocSubject]int64
	tenants   map[int64]int64
	tenantCap map[int64]int64

	playerRejected atomic.Int64
	tenantRejected atomic.Int64
}

type allocSubject struct{ tenantID, playerID int64 }

func newSubjectAllocCaps(perPlayer, defaultTenant int) *subjectAllocCaps {
	return &subjectAllocCaps{
		perPlayer:     int64(perPlayer),
		defaultTenant: int64(defaultTenant),
		players:       make(map[allocSubject]int64),
		tenants:       make(map[int64]int64),
		tenantCap:     make(map[int64]int64),
	}
}

// setTenantCap records the cap carried by a verified credential. allocCap <= 0
// means the credential carried none.
func (c *subjectAllocCaps) setTenantCap(tenantID, allocCap int64) {
	if allocCap <= 0 {
		allocCap = c.defaultTenant
	}
	c.mu.Lock()
	c.tenantCap[tenantID] = allocCap
	c.mu.Unlock()
}

// check reports whether one more allocation would cross a cap, without
// reserving it.
func (c *subjectAllocCaps) check(tenantID, playerID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refuseLocked(tenantID, playerID)
}

// acquire reserves one allocation for the subject, or refuses it.
func (c *subjectAllocCaps) acquire(tenantID, playerID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refuseLocked(tenantID, playerID); err != nil {
		return err
	}
	c.players[allocSubject{tenantID, playerID}]++
	c.tenants[tenantID]++
	return nil
}

// release returns one allocation acquired for the subject.
func (c *subjectAllocCaps) release(tenantID, playerID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := allocSubject{tenantID, playerID}
	if c.players[s]--; c.players[s] <= 0 {
		delete(c.players, s)
	}
	if c.tenants[tenantID]--; c.tenants[tenantID] <= 0 {
		delete(c.tenants, tenantID)
	}
}

func (c *subjectAllocCaps) refuseLocked(tenantID, playerID int64) error {
	if c.perPlayer > 0 && c.players[allocSubject{tenantID, playerID}] >= c.perPlayer {
		c.playerRejected.Add(1)
		return errPlayerAllocationCap
	}
	limit, ok := c.tenantCap[tenantID]
	if !ok {
		limit = c.defaultTenant
	}
	if limit > 0 && c.tenants[tenantID] >= limit {
		c.tenantRejected.Add(1)
		return errTenantAllocationCap
	}
	return nil
}
