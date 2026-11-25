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

func newTestCache(inner Lookup, ttl time.Duration, maxEntries int, now *time.Time) *KeyCache {
	return &KeyCache{inner: inner, ttl: ttl, max: maxEntries, now: func() time.Time { return *now }, entries: make(map[string]keyCacheEntry)}
}

func TestCachedLookup_should_skip_inner_lookup_when_key_is_cached(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.Lookup(context.Background(), []byte("k"))
	key, err := c.Lookup(context.Background(), []byte("k"))

	require.NoError(t, err)
	require.Equal(t, int64(7), key.ID)
	assert.Equal(t, 1, calls)
}

func TestCachedLookup_should_call_inner_lookup_when_hash_differs(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.Lookup(context.Background(), []byte("a"))
	_, _ = c.Lookup(context.Background(), []byte("b"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_reload_when_entry_expired(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	_, _ = c.Lookup(context.Background(), []byte("k"))
	now = now.Add(30 * time.Second)
	_, _ = c.Lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_cache_unknown_key(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(nil, &calls), 30*time.Second, 10, &now)

	_, _ = c.Lookup(context.Background(), []byte("bad"))
	_, err := c.Lookup(context.Background(), []byte("bad"))

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

	_, _ = c.Lookup(context.Background(), []byte("k"))
	_, _ = c.Lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestCachedLookup_should_return_a_copy_of_the_cached_key(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)

	first, _ := c.Lookup(context.Background(), []byte("k"))
	first.Hash = []byte("changed")
	second, _ := c.Lookup(context.Background(), []byte("k"))

	assert.Nil(t, second.Hash)
}

func TestCachedLookup_should_stay_within_max_entries(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 2, &now)

	for _, k := range []string{"a", "b", "c", "d"} {
		_, _ = c.Lookup(context.Background(), []byte(k))
	}

	assert.Len(t, c.entries, 2)
}

func TestCachedLookup_should_ask_miss_gate_only_on_a_miss(t *testing.T) {
	calls, gates := 0, 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)
	ctx := context.WithValue(context.Background(), missGateKey{}, func() bool { gates++; return true })

	_, _ = c.Lookup(ctx, []byte("k"))
	_, err := c.Lookup(ctx, []byte("k"))

	require.NoError(t, err)
	assert.Equal(t, 1, gates)
	assert.Equal(t, 1, calls)
}

func TestCachedLookup_should_not_call_inner_or_cache_when_miss_gate_refuses(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7}, &calls), 30*time.Second, 10, &now)
	refuse := context.WithValue(context.Background(), missGateKey{}, func() bool { return false })

	_, err := c.Lookup(refuse, []byte("k"))
	require.ErrorIs(t, err, errMissRefused)
	_, err = c.Lookup(context.Background(), []byte("k"))

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestKeyCache_should_reload_key_after_invalidate_for_its_tenant(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7, TenantID: 3}, &calls), 30*time.Second, 10, &now)
	_, _ = c.Lookup(context.Background(), []byte("k"))

	c.Invalidate(3)
	_, _ = c.Lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestKeyCache_should_keep_keys_of_other_tenants_when_invalidated(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(&APIKey{ID: 7, TenantID: 3}, &calls), 30*time.Second, 10, &now)
	_, _ = c.Lookup(context.Background(), []byte("k"))

	c.Invalidate(4)
	_, _ = c.Lookup(context.Background(), []byte("k"))

	assert.Equal(t, 1, calls)
}

func TestKeyCache_should_keep_unknown_keys_when_invalidated(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	c := newTestCache(countingLookup(nil, &calls), 30*time.Second, 10, &now)
	_, _ = c.Lookup(context.Background(), []byte("bad"))

	c.Invalidate(3)
	_, _ = c.Lookup(context.Background(), []byte("bad"))

	assert.Equal(t, 1, calls)
}

func TestKeyCache_should_not_store_a_lookup_that_ran_during_invalidate(t *testing.T) {
	calls := 0
	now := time.Unix(1000, 0)
	var c *KeyCache
	c = newTestCache(func(_ context.Context, _ []byte) (*APIKey, error) {
		calls++
		// The write commits and drops the tenant while this read is in flight.
		c.Invalidate(3)
		return &APIKey{ID: 7, TenantID: 3}, nil
	}, 30*time.Second, 10, &now)

	_, _ = c.Lookup(context.Background(), []byte("k"))
	_, _ = c.Lookup(context.Background(), []byte("k"))

	assert.Equal(t, 2, calls)
}

func TestKeyCache_should_ignore_invalidate_on_a_nil_cache(t *testing.T) {
	var c *KeyCache

	assert.NotPanics(t, func() { c.Invalidate(3) })
}

func TestNewKeyCache_should_use_default_ttl_when_ttl_is_zero(t *testing.T) {
	calls := 0
	c := NewKeyCache(countingLookup(&APIKey{ID: 7}, &calls), 0, 10)

	assert.Equal(t, DefaultAPIKeyCacheTTL, c.ttl)
}
