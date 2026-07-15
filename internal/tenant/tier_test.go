package tenant_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/tenant"
)

func TestClampTier_maps_in_range_values(t *testing.T) {
	cases := []struct {
		in   int
		want tenant.Tier
	}{
		{0, tenant.Tier0},
		{1, tenant.Tier1},
		{2, tenant.Tier2},
		{3, tenant.Tier3},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tenant.ClampTier(tc.in))
	}
}

func TestClampTier_out_of_range_falls_back_to_tier0(t *testing.T) {
	for _, in := range []int{-1, 4, 99, -100} {
		assert.Equal(t, tenant.Tier0, tenant.ClampTier(in), "in=%d", in)
	}
}

func TestTier_String_renders_numbered_class(t *testing.T) {
	cases := []struct {
		in   tenant.Tier
		want string
	}{
		{tenant.Tier0, "tier_0"},
		{tenant.Tier1, "tier_1"},
		{tenant.Tier2, "tier_2"},
		{tenant.Tier3, "tier_3"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.in.String())
	}
}

func TestTier_String_out_of_range_renders_tier0(t *testing.T) {
	assert.Equal(t, "tier_0", tenant.Tier(99).String())
}
