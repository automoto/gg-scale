package managementtoken

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

var cacheNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestCache_should_return_a_row_before_the_ttl_ends(t *testing.T) {
	// Arrange
	c := NewCache(2*time.Second, 10)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 7}, cacheNow)

	// Act
	row, ok := c.get([32]byte{1}, cacheNow.Add(1999*time.Millisecond))

	// Assert
	assert.Equal(t, []any{true, int64(7)}, []any{ok, row.ID})
}

func TestCache_should_miss_when_the_ttl_ends(t *testing.T) {
	// Arrange
	c := NewCache(2*time.Second, 10)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 7}, cacheNow)

	// Act
	_, ok := c.get([32]byte{1}, cacheNow.Add(2*time.Second))

	// Assert
	assert.False(t, ok)
}

func TestCache_should_be_off_for_a_zero_ttl(t *testing.T) {
	// Arrange
	c := NewCache(0, 10)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 7}, cacheNow)

	// Act
	_, ok := c.get([32]byte{1}, cacheNow)

	// Assert
	assert.False(t, ok)
}

func TestCache_put_should_remove_ended_entries(t *testing.T) {
	// Arrange
	c := NewCache(2*time.Second, 10)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 1}, cacheNow)

	// Act
	c.put([32]byte{2}, sqlcgen.GetManagementTokenByHashRow{ID: 2}, cacheNow.Add(3*time.Second))

	// Assert
	assert.Len(t, c.entries, 1)
}

func TestCache_touched_should_set_last_used(t *testing.T) {
	// Arrange
	c := NewCache(2*time.Second, 10)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 7}, cacheNow)

	// Act
	c.touched([32]byte{1}, cacheNow)
	row, _ := c.get([32]byte{1}, cacheNow)

	// Assert
	assert.Equal(t, pgtype.Timestamptz{Time: cacheNow, Valid: true}, row.LastUsedAt)
}

func TestCache_put_should_not_exceed_max_entries(t *testing.T) {
	// Arrange
	c := NewCache(time.Hour, 3)

	// Act
	for i := byte(1); i <= 10; i++ {
		c.put([32]byte{i}, sqlcgen.GetManagementTokenByHashRow{ID: int64(i)}, cacheNow)
	}

	// Assert
	assert.Len(t, c.entries, 3)
}

func TestCache_put_should_keep_the_new_entry_when_full(t *testing.T) {
	// Arrange
	c := NewCache(time.Hour, 2)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 1}, cacheNow)
	c.put([32]byte{2}, sqlcgen.GetManagementTokenByHashRow{ID: 2}, cacheNow)

	// Act
	c.put([32]byte{3}, sqlcgen.GetManagementTokenByHashRow{ID: 3}, cacheNow)
	_, ok := c.get([32]byte{3}, cacheNow)

	// Assert
	assert.True(t, ok)
}

func TestCache_put_should_evict_the_entry_that_ends_first_when_full(t *testing.T) {
	// Arrange
	c := NewCache(time.Hour, 2)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 1}, cacheNow)
	c.put([32]byte{2}, sqlcgen.GetManagementTokenByHashRow{ID: 2}, cacheNow.Add(time.Minute))

	// Act
	c.put([32]byte{3}, sqlcgen.GetManagementTokenByHashRow{ID: 3}, cacheNow.Add(2*time.Minute))
	_, ok := c.get([32]byte{1}, cacheNow.Add(2*time.Minute))

	// Assert
	assert.False(t, ok)
}

func TestCache_put_should_replace_an_entry_without_eviction_when_full(t *testing.T) {
	// Arrange
	c := NewCache(time.Hour, 2)
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 1}, cacheNow)
	c.put([32]byte{2}, sqlcgen.GetManagementTokenByHashRow{ID: 2}, cacheNow)

	// Act
	c.put([32]byte{1}, sqlcgen.GetManagementTokenByHashRow{ID: 9}, cacheNow)

	// Assert
	assert.Len(t, c.entries, 2)
}
