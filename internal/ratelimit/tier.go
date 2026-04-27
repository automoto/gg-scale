package ratelimit

import "github.com/ggscale/ggscale/internal/tenant"

// Limits captures the token-bucket parameters for a tier.
type Limits struct {
	RatePerSecond float64 // tokens added to the bucket per second
	Burst         float64 // maximum tokens (and bucket capacity)
}

// LimitsForTier returns the token-bucket parameters for the given tenant
// tier. Premium currently uses a static high default; per-tenant overrides
// follow as a v1.1 enhancement.
//
// Unknown tiers fall back to Free — fail-closed against tier-string typos.
func LimitsForTier(t tenant.Tier) Limits {
	switch t {
	case tenant.TierFree:
		return Limits{RatePerSecond: 60, Burst: 60}
	case tenant.TierPAYG:
		return Limits{RatePerSecond: 600, Burst: 600}
	case tenant.TierPremium:
		return Limits{RatePerSecond: 6000, Burst: 6000}
	default:
		return Limits{RatePerSecond: 60, Burst: 60}
	}
}
