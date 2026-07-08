package storagelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory LimitStore that counts Resolve calls so tests can
// assert on cache hits/misses without a database.
type fakeStore struct {
	value        int64
	resolveCalls int
	setCalls     int
}

func (f *fakeStore) Resolve(_ context.Context, _, _, _ int64) (int64, error) {
	f.resolveCalls++
	return f.value, nil
}

func (f *fakeStore) Set(_ context.Context, _, _ int64, _ *int64, maxBytes int64) error {
	f.setCalls++
	f.value = maxBytes
	return nil
}

func (f *fakeStore) ListForTenant(_ context.Context, _ int64) ([]Override, error) {
	return nil, nil
}

func TestCachedStore_serves_from_cache_within_ttl(t *testing.T) {
	fake := &fakeStore{value: 100}
	c := NewCachedStore(fake, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	first, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)
	second, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)

	assert.Equal(t, int64(100), first)
	assert.Equal(t, int64(100), second)
	assert.Equal(t, 1, fake.resolveCalls, "second read within TTL should hit the cache")
}

func TestCachedStore_reloads_after_ttl_expires(t *testing.T) {
	fake := &fakeStore{value: 100}
	c := NewCachedStore(fake, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)
	now = now.Add(2 * time.Minute)
	_, err = c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)

	assert.Equal(t, 2, fake.resolveCalls, "read after TTL should reload from the inner store")
}

func TestCachedStore_set_invalidates_tenant_entries(t *testing.T) {
	fake := &fakeStore{value: 100}
	c := NewCachedStore(fake, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, 0, 1, nil, 250))

	got, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)
	assert.Equal(t, int64(250), got, "read after Set should reflect the new value, not the cached one")
	assert.Equal(t, 2, fake.resolveCalls, "Set must invalidate the cached entry so the next read reloads")
}

func TestCachedStore_default_key_isolates_callers(t *testing.T) {
	fake := &fakeStore{value: 100}
	c := NewCachedStore(fake, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := c.Resolve(ctx, 1, 2, 999)
	require.NoError(t, err)
	_, err = c.Resolve(ctx, 1, 2, 500)
	require.NoError(t, err)

	assert.Equal(t, 2, fake.resolveCalls, "a different default is a distinct cache key")
}

func TestCachedStore_invalidate_scopes_to_one_tenant(t *testing.T) {
	fake := &fakeStore{value: 100}
	c := NewCachedStore(fake, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := c.Resolve(ctx, 12, 0, 999)
	require.NoError(t, err)
	_, err = c.Resolve(ctx, 123, 0, 999)
	require.NoError(t, err)

	// Writing tenant 12 must not drop tenant 123's cached entry (prefix guard:
	// "12:" must not match "123:").
	require.NoError(t, c.Set(ctx, 0, 12, nil, 250))
	_, err = c.Resolve(ctx, 123, 0, 999)
	require.NoError(t, err)

	assert.Equal(t, 2, fake.resolveCalls, "tenant 123 stays cached when only tenant 12 is invalidated")
}
