package realtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestTenantCapLimits(t *testing.T) {
	t.Run("uses the tier-class envelope by default", func(t *testing.T) {
		for _, tier := range []tenant.Tier{tenant.Tier0, tenant.Tier1, tenant.Tier2, tenant.Tier3} {
			got := tenantCapLimits(tier, 0)
			assert.Equal(t, ratelimit.ConnectionCapForClass(tier), got, "tier %d", tier)
		}
	})

	t.Run("env override pins a fixed hard cap with no burst headroom", func(t *testing.T) {
		got := tenantCapLimits(tenant.Tier3, 250)
		assert.Equal(t, ratelimit.CapLimits{Sustained: 250, Ceiling: 250}, got,
			"override ignores the tier and disables burst (sustained == ceiling)")
	})
}
