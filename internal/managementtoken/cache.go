package managementtoken

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// Cache keeps the database part of a successful token check for a short
// time, so the inner request of an MCP tool call does not look up the token
// again. A revoked token, a disabled tenant or creator, and a deleted project
// take effect within the TTL on each app instance; there is no invalidation
// across instances. The expiry time, the creator permission, and the rate
// limit are checked on each request. Only successful checks are kept, so the
// size is limited by the number of live tokens.
type Cache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[[32]byte]cacheEntry
}

type cacheEntry struct {
	row     sqlcgen.GetManagementTokenByHashRow
	expires time.Time
}

// NewCache returns a cache that keeps entries for ttl. A ttl of 0 or less
// returns nil, which caches nothing.
func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		return nil
	}
	return &Cache{ttl: ttl, entries: map[[32]byte]cacheEntry{}}
}

func (c *Cache) get(hash [32]byte, now time.Time) (sqlcgen.GetManagementTokenByHashRow, bool) {
	if c == nil {
		return sqlcgen.GetManagementTokenByHashRow{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[hash]
	if !ok || !now.Before(e.expires) {
		return sqlcgen.GetManagementTokenByHashRow{}, false
	}
	return e.row, true
}

// put adds row and removes the entries whose time has ended.
func (c *Cache) put(hash [32]byte, row sqlcgen.GetManagementTokenByHashRow, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[hash] = cacheEntry{row: row, expires: now.Add(c.ttl)}
}

// touched records a last_used_at write, so a cached row does not cause the
// same write again before the entry ends.
func (c *Cache) touched(hash [32]byte, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[hash]; ok {
		e.row.LastUsedAt = pgtype.Timestamptz{Time: now, Valid: true}
		c.entries[hash] = e
	}
}
