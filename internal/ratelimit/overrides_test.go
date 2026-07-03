package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

type countingOverrides struct {
	apiCalls    int
	inviteCalls int
	apiLimit    ratelimit.Limits
	apiFound    bool
}

func (c *countingOverrides) APILimit(_ context.Context, _ int64) (ratelimit.Limits, bool, error) {
	c.apiCalls++
	return c.apiLimit, c.apiFound, nil
}

func (c *countingOverrides) InviteLimit(_ context.Context, _, _ int64, _ string) (ratelimit.Limits, bool, error) {
	c.inviteCalls++
	return ratelimit.Limits{}, false, nil
}

func TestCachedOverrideStore_memoizes_within_ttl(t *testing.T) {
	inner := &countingOverrides{apiLimit: ratelimit.Limits{RatePerSecond: 5, Burst: 5}, apiFound: true}
	cached := ratelimit.NewCachedOverrideStore(inner, ratelimit.DefaultOverrideCacheTTL)

	for i := 0; i < 3; i++ {
		limits, ok, err := cached.APILimit(context.Background(), 42)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 5.0, limits.Burst)
	}
	assert.Equal(t, 1, inner.apiCalls, "repeated lookups within the TTL hit the backend once")
}

func TestCachedOverrideStore_keys_by_tenant(t *testing.T) {
	inner := &countingOverrides{apiFound: false}
	cached := ratelimit.NewCachedOverrideStore(inner, ratelimit.DefaultOverrideCacheTTL)

	_, _, _ = cached.APILimit(context.Background(), 1)
	_, _, _ = cached.APILimit(context.Background(), 2)
	assert.Equal(t, 2, inner.apiCalls, "distinct tenants are cached separately")
}

func TestCachedOverrideStore_invalidate_forces_reread(t *testing.T) {
	// Long TTL: only Invalidate can refresh, isolating the invalidation path.
	inner := &countingOverrides{apiLimit: ratelimit.Limits{Burst: 5}, apiFound: true}
	cached := ratelimit.NewCachedOverrideStore(inner, time.Hour)

	_, _, _ = cached.APILimit(context.Background(), 42)
	_, _, _ = cached.APILimit(context.Background(), 42)
	require.Equal(t, 1, inner.apiCalls, "second read served from cache")

	cached.Invalidate(42)
	_, _, _ = cached.APILimit(context.Background(), 42)
	assert.Equal(t, 2, inner.apiCalls, "invalidate forces a fresh backend read")
}

func TestCachedOverrideStore_invalidate_clears_invite_entries(t *testing.T) {
	inner := &countingOverrides{}
	cached := ratelimit.NewCachedOverrideStore(inner, time.Hour)

	_, _, _ = cached.InviteLimit(context.Background(), 7, 3, ratelimit.OverrideKindInviteInviter)
	_, _, _ = cached.InviteLimit(context.Background(), 7, 3, ratelimit.OverrideKindInviteInviter)
	require.Equal(t, 1, inner.inviteCalls)

	cached.Invalidate(7)
	_, _, _ = cached.InviteLimit(context.Background(), 7, 3, ratelimit.OverrideKindInviteInviter)
	assert.Equal(t, 2, inner.inviteCalls, "per-project invite entries dropped on invalidate")
}

func TestCachedOverrideStore_invalidate_scoped_to_tenant(t *testing.T) {
	inner := &countingOverrides{apiFound: false}
	cached := ratelimit.NewCachedOverrideStore(inner, time.Hour)

	_, _, _ = cached.APILimit(context.Background(), 1)
	_, _, _ = cached.APILimit(context.Background(), 2)
	require.Equal(t, 2, inner.apiCalls)

	cached.Invalidate(1)
	_, _, _ = cached.APILimit(context.Background(), 1) // dropped → re-read
	_, _, _ = cached.APILimit(context.Background(), 2) // untouched → still cached
	assert.Equal(t, 3, inner.apiCalls, "only the invalidated tenant re-reads")
}
