package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingLookup returns key (or ErrUnknownKey when nil) and counts calls.
func countingLookup(key *APIKey, calls *int) Lookup {
	return func(_ context.Context, _ []byte) (*APIKey, error) {
		*calls++
		if key == nil {
			return nil, ErrUnknownKey
		}
		return key, nil
	}
}

func newTestCache(inner Lookup, ttl time.Duration, maxEntries int, now *time.Time) *keyCache {
	return &keyCache{inner: inner, ttl: ttl, max: maxEntries, now: func() time.Time { return *now }, entries: make(map[string]keyCacheEntry)}
}

func TestCachedLookup_should_skip_inner_lookup_when_key_is_cached(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.lookup(context.Background(), []byte("k"))
	key, err := c.lookup(context.Background(), []byte("k"))

	require.NoError(t, err)
	require.Equal(t, int64(7), key.ID)
	assert.Equal(t, 1, calls)
}

func TestCachedLookup_should_call_inner_lookup_when_hash_differs(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.lookup(context.Background(), []byte("a"))
	_, _ = c.lookup(context.Background(), []byte("b"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_reload_when_entry_expired(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.lookup(context.Background(), []byte("k"))
	now = now.Add(30 * time.Second)
	_, _ = c.lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_cache_unknown_key(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(nil, &calls), 30*time.Second, 10, &now)

	_, _ = c.lookup(context.Background(), []byte("bad"))
	_, err := c.lookup(context.Background(), []byte("bad"))

	require.ErrorIs(t, err, ErrUnknownKey)
	assert.Equal(t, 1, calls)
}

func TestCachedLookup_should_not_cache_lookup_errors(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	inner := func(_ context.Context, _ []byte) (*APIKey, error) {
		calls++
		return nil, errors.New("db down")
	}
	c := newTestCache(inner, 30*time.Second, 10, &now)

	_, _ = c.lookup(context.Background(), []byte("k"))
	_, _ = c.lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_return_a_copy_of_the_cached_key(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	first, _ := c.lookup(context.Background(), []byte("k"))
	first.Hash = []byte("changed")
	second, _ := c.lookup(context.Background(), []byte("k"))

	assert.Nil(t, second.Hash)
}

func TestCachedLookup_should_stay_within_max_entries(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 2, &now)

	for _, k := range []string{"a", "b", "c", "d"} {
		_, _ = c.lookup(context.Background(), []byte(k))
	}

	assert.Len(t, c.entries, 2)
}
