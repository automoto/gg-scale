package ratelimit

import "github.com/automoto/gg-scale/internal/tenant"

// Limits captures the token-bucket parameters for a tier.
type Limits struct {
	RatePerSecond float64 // tokens added to the bucket per second
	Burst         float64 // maximum tokens (and bucket capacity)
}

// LimitsForTier returns the token-bucket parameters for the given tenant
// class. Sizing rule: sustained rate = the class CCU cap / 10, so a chatty
// game (one DB action per player per 10 s) can fill its advertised connection
// cap without the rate axis binding first. Burst capacity covers one immediate
// backend action
// per sustained connection, so a full key can absorb a synchronized login or
// reconnect wave before settling to the sustained refill rate. tier_3 values
// are the defaults an operator starts from before applying per-axis overrides.
//
// Unknown/out-of-range classes fall back to tier_0 — fail-closed.
func LimitsForTier(t tenant.Tier) Limits {
	switch t {
	case tenant.Tier1:
		return Limits{RatePerSecond: 1000, Burst: 10000}
	case tenant.Tier2:
		return Limits{RatePerSecond: 5000, Burst: 50000}
	case tenant.Tier3:
		return Limits{RatePerSecond: 10000, Burst: 100000}
	default:
		return Limits{RatePerSecond: 250, Burst: 2500}
	}
}
