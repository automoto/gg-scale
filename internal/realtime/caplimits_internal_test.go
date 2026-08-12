package realtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestTenantCapLimits(t *testing.T) {
	t.Run("uses the tier-class envelope by default", func(t *testing.T) {
		for _, tier := range []tenant.Tier{tenant.Tier0, tenant.Tier1, tenant.Tier2, tenant.Tier3} {
			got := tenantCapLimits(tenant.APIKey{Tier: tier}, 0)
			assert.Equal(t, ratelimit.ConnectionCapForClass(tier), got, "tier %d", tier)
		}
	})

	t.Run("uses the override resolved with the API key", func(t *testing.T) {
		key := tenant.APIKey{
			Tier: tenant.Tier2,
			ConnectionLimits: &tenant.ConnectionLimits{
				Sustained: 10_000,
				Ceiling:   20_000,
			},
		}

		got := tenantCapLimits(key, 0)

		assert.Equal(t, ratelimit.CapLimits{Sustained: 10_000, Ceiling: 20_000}, got,
			"a lower tenant override must not be widened to its tier default")
	})

	t.Run("env override pins a fixed hard cap with no burst headroom", func(t *testing.T) {
		key := tenant.APIKey{
			Tier:             tenant.Tier3,
			ConnectionLimits: &tenant.ConnectionLimits{Sustained: 1, Ceiling: 2},
		}

		got := tenantCapLimits(key, 250)

		assert.Equal(t, ratelimit.CapLimits{Sustained: 250, Ceiling: 250}, got,
			"override ignores the tier and disables burst (sustained == ceiling)")
	})
}
