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
// The tenant cap comes from the newest verified credential of that tenant that
// carries one (see Issuer.IssueWithTenantCap); until then defaultTenant applies.
// It is forgotten with the tenant's last allocation.
// Tenant caps use the credential's values: unlimitedTenantCap lifts the cap,
// 0 refuses every new allocation.
type subjectAllocCaps struct {
	perPlayer     int64 // 0 = unlimited
	defaultTenant int64

	mu        sync.Mutex
	players   map[allocSubject]int64
	tenants   map[int64]int64
	tenantCap map[int64]tenantCapEntry

	playerRejected atomic.Int64
	tenantRejected atomic.Int64
}

type allocSubject struct{ tenantID, playerID int64 }

// tenantCapEntry is a tenant's cap and the expiry of the credential that set
// it. Issuers share one TTL, so a later expiry means a newer credential.
type tenantCapEntry struct{ limit, expires int64 }

// unlimitedTenantCap is the credential value that lifts the per-tenant cap. It
// matches quota.Unlimited, which the issuer passes through unchanged.
const unlimitedTenantCap = -1

// newSubjectAllocCaps takes env-style caps: <= 0 means unlimited.
func newSubjectAllocCaps(perPlayer, defaultTenant int) *subjectAllocCaps {
	tenantDefault := int64(defaultTenant)
	if tenantDefault <= 0 {
		tenantDefault = unlimitedTenantCap
	}
	return &subjectAllocCaps{
		perPlayer:     int64(perPlayer),
		defaultTenant: tenantDefault,
		players:       make(map[allocSubject]int64),
		tenants:       make(map[int64]int64),
		tenantCap:     make(map[int64]tenantCapEntry),
	}
}

// setTenantCap records the cap carried by a verified credential with the given
// expiry. A nil cap, or a credential older than the recorded one, changes nothing.
func (c *subjectAllocCaps) setTenantCap(tenantID, expires int64, allocCap *int64) {
	// A credential without the field (issued before the rollout flag) and an
	// older credential must not weaken a cap set by a newer one, for example
	// after a platform admin lowers the tenant's override.
	if allocCap == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.tenantCap[tenantID]; ok && expires < e.expires {
		return
	}
	c.tenantCap[tenantID] = tenantCapEntry{limit: *allocCap, expires: expires}
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
		delete(c.tenantCap, tenantID) // the next Allocate's credential sets it again
	}
}

func (c *subjectAllocCaps) refuseLocked(tenantID, playerID int64) error {
	if c.perPlayer > 0 && c.players[allocSubject{tenantID, playerID}] >= c.perPlayer {
		c.playerRejected.Add(1)
		return errPlayerAllocationCap
	}
	limit := c.defaultTenant
	if e, ok := c.tenantCap[tenantID]; ok {
		limit = e.limit
	}
	if limit != unlimitedTenantCap && c.tenants[tenantID] >= limit {
		c.tenantRejected.Add(1)
		return errTenantAllocationCap
	}
	return nil
}
