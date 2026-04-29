package ratelimit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestConnectionCapForTier_distinguishes_known_tiers(t *testing.T) {
	free := ratelimit.ConnectionCapForTier(tenant.TierFree)
	payg := ratelimit.ConnectionCapForTier(tenant.TierPAYG)
	premium := ratelimit.ConnectionCapForTier(tenant.TierPremium)

	assert.Greater(t, payg, free)
	assert.Greater(t, premium, payg)
}

func TestConnectionCapForTier_unknown_tier_falls_back_to_free(t *testing.T) {
	got := ratelimit.ConnectionCapForTier("never-heard-of-it")
	free := ratelimit.ConnectionCapForTier(tenant.TierFree)

	assert.Equal(t, free, got)
}

func TestCloseCodeTenantConnectionCap_is_RFC_6455_try_again_later(t *testing.T) {
	// RFC 6455 §7.4.1: 1013 = "Try Again Later".
	assert.Equal(t, 1013, ratelimit.CloseCodeTenantConnectionCap)
}

func TestCloseReasonTenantConnectionCap_matches_m1_spec(t *testing.T) {
	assert.Equal(t, "tenant_connection_cap", ratelimit.CloseReasonTenantConnectionCap)
}
