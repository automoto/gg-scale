package ratelimit

import "github.com/ggscale/ggscale/internal/tenant"

// Limits captures the token-bucket parameters for a tier.
type Limits struct {
	RatePerSecond float64 // tokens added to the bucket per second
	Burst         float64 // maximum tokens (and bucket capacity)
}

// LimitsForTier returns the token-bucket parameters for the given tenant
// class. Sustained req/s per the tier-rework ladder; burst (bucket capacity)
// is 2× the sustained rate so login spikes and reconnect storms absorb into
// the bucket. tier_3 values are the defaults an operator starts from before
// applying per-axis overrides (latent per pricing-strategy.md).
//
// Unknown/out-of-range classes fall back to tier_0 — fail-closed.
func LimitsForTier(t tenant.Tier) Limits {
	switch t {
	case tenant.Tier1:
		return Limits{RatePerSecond: 1000, Burst: 2000}
	case tenant.Tier2:
		return Limits{RatePerSecond: 5000, Burst: 10000}
	case tenant.Tier3:
		return Limits{RatePerSecond: 10000, Burst: 20000}
	default:
		return Limits{RatePerSecond: 150, Burst: 300}
	}
}
