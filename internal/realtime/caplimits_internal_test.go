package realtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

type stubConnectionLimits struct {
	limits ratelimit.CapLimits
	found  bool
	err    error
	calls  int
}

func (s *stubConnectionLimits) ConnectionLimit(context.Context, int64) (ratelimit.CapLimits, bool, error) {
	s.calls++
	return s.limits, s.found, s.err
}

func TestTenantCapLimits(t *testing.T) {
	t.Run("uses the tier-class envelope by default", func(t *testing.T) {
		for _, tier := range []tenant.Tier{tenant.Tier0, tenant.Tier1, tenant.Tier2, tenant.Tier3} {
			got, err := tenantCapLimits(context.Background(), 42, tier, 0, nil)
			require.NoError(t, err)
			assert.Equal(t, ratelimit.ConnectionCapForClass(tier), got, "tier %d", tier)
		}
	})

	t.Run("uses a tenant-specific override", func(t *testing.T) {
		store := &stubConnectionLimits{
			limits: ratelimit.CapLimits{Sustained: 250_000, Ceiling: 500_000},
			found:  true,
		}
		got, err := tenantCapLimits(context.Background(), 42, tenant.Tier2, 0, store)
		require.NoError(t, err)
		assert.Equal(t, store.limits, got)
		assert.Equal(t, 1, store.calls)
	})

	t.Run("env override pins a fixed hard cap with no burst headroom", func(t *testing.T) {
		store := &stubConnectionLimits{
			limits: ratelimit.CapLimits{Sustained: 1, Ceiling: 2},
			found:  true,
		}
		got, err := tenantCapLimits(context.Background(), 42, tenant.Tier3, 250, store)
		require.NoError(t, err)
		assert.Equal(t, ratelimit.CapLimits{Sustained: 250, Ceiling: 250}, got,
			"override ignores the tier and disables burst (sustained == ceiling)")
		assert.Zero(t, store.calls, "deployment-wide override takes precedence without a DB lookup")
	})

	t.Run("returns the tier default when an override cannot be resolved", func(t *testing.T) {
		store := &stubConnectionLimits{err: errors.New("database unavailable")}
		got, err := tenantCapLimits(context.Background(), 42, tenant.Tier2, 0, store)
		assert.ErrorContains(t, err, "connection limit override")
		assert.Equal(t, ratelimit.ConnectionCapForClass(tenant.Tier2), got)
	})

	t.Run("returns a stale override with the lookup error", func(t *testing.T) {
		store := &stubConnectionLimits{
			limits: ratelimit.CapLimits{Sustained: 250_000, Ceiling: 500_000},
			found:  true,
			err:    errors.New("database unavailable"),
		}
		got, err := tenantCapLimits(context.Background(), 42, tenant.Tier2, 0, store)
		assert.ErrorContains(t, err, "connection limit override")
		assert.Equal(t, store.limits, got)
	})
}
