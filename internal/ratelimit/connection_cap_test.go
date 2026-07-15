package ratelimit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestConnectionCapForClass_climbs_with_class(t *testing.T) {
	t0 := ratelimit.ConnectionCapForClass(tenant.Tier0)
	t1 := ratelimit.ConnectionCapForClass(tenant.Tier1)
	t2 := ratelimit.ConnectionCapForClass(tenant.Tier2)
	t3 := ratelimit.ConnectionCapForClass(tenant.Tier3)

	assert.Equal(t, int64(5000), t0.Sustained)
	assert.Greater(t, t1.Sustained, t0.Sustained)
	assert.Greater(t, t2.Sustained, t1.Sustained)
	assert.GreaterOrEqual(t, t3.Sustained, t2.Sustained)
}

func TestConnectionCapForClass_ceiling_is_double_sustained(t *testing.T) {
	for _, tier := range []tenant.Tier{tenant.Tier0, tenant.Tier1, tenant.Tier2, tenant.Tier3} {
		caps := ratelimit.ConnectionCapForClass(tier)
		assert.Equal(t, 2*caps.Sustained, caps.Ceiling, "tier=%s", tier)
	}
}

func TestConnectionCapForClass_unknown_class_falls_back_to_tier0(t *testing.T) {
	got := ratelimit.ConnectionCapForClass(tenant.Tier(99))
	t0 := ratelimit.ConnectionCapForClass(tenant.Tier0)

	assert.Equal(t, t0, got)
}

func TestCacheConnectionCap_admits_burst_up_to_ceiling(t *testing.T) {
	store := memory.New()
	defer func() { _ = store.Close(context.Background()) }()
	cap := ratelimit.NewCacheConnectionCap(store, nil)
	caps := ratelimit.CapLimits{Sustained: 3, Ceiling: 6}

	// With a full budget at one instant, connections up to the ceiling admit.
	for i := 0; i < 6; i++ {
		d, err := cap.Acquire(context.Background(), "wsconn:1", caps)
		require.NoError(t, err)
		assert.True(t, d.Allowed, "connection %d within burst envelope", i+1)
	}
}

func TestCacheConnectionCap_ceiling_rejects_with_reason(t *testing.T) {
	store := memory.New()
	defer func() { _ = store.Close(context.Background()) }()
	cap := ratelimit.NewCacheConnectionCap(store, nil)
	caps := ratelimit.CapLimits{Sustained: 2, Ceiling: 4}

	// Fill to the ceiling in one instant (budget is full, so burst is allowed).
	for i := 0; i < 4; i++ {
		d, err := cap.Acquire(context.Background(), "wsconn:2", caps)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	// The next connection hits the hard ceiling.
	d, err := cap.Acquire(context.Background(), "wsconn:2", caps)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, ratelimit.CapRejectCeiling, d.Reason)
}

type deterministicCapRejectionStore struct {
	cache.Store
}

func (deterministicCapRejectionStore) AcquireSlotBurst(_ context.Context, key string, _, ceiling int64, _, _ time.Duration) (bool, int64, error) {
	if key == "ceiling" {
		return false, ceiling, nil
	}
	return false, ceiling - 1, nil
}

func TestCacheConnectionCap_rejection_metrics_distinguish_ceiling_and_budget_without_ids(t *testing.T) {
	reg := prometheus.NewRegistry()
	cap := ratelimit.NewCacheConnectionCap(deterministicCapRejectionStore{}, reg)
	caps := ratelimit.CapLimits{Sustained: 2, Ceiling: 4}

	ceiling, err := cap.Acquire(context.Background(), "ceiling", caps)
	require.NoError(t, err)
	budget, err := cap.Acquire(context.Background(), "budget", caps)
	require.NoError(t, err)
	assert.Equal(t, ratelimit.CapRejectCeiling, ceiling.Reason)
	assert.Equal(t, ratelimit.CapRejectBudget, budget.Reason)

	expected := `
# HELP ggscale_connection_cap_rejections_total WebSocket connections rejected by the tenant CCU cap, by reason (ceiling vs exhausted burst budget).
# TYPE ggscale_connection_cap_rejections_total counter
ggscale_connection_cap_rejections_total{reason="budget"} 1
ggscale_connection_cap_rejections_total{reason="ceiling"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"ggscale_connection_cap_rejections_total"))
}

func TestCloseCodeTenantConnectionCap_is_RFC_6455_try_again_later(t *testing.T) {
	// RFC 6455 §7.4.1: 1013 = "Try Again Later".
	assert.Equal(t, 1013, ratelimit.CloseCodeTenantConnectionCap)
}

func TestCloseReasonTenantConnectionCap_matches_m1_spec(t *testing.T) {
	assert.Equal(t, "tenant_connection_cap", ratelimit.CloseReasonTenantConnectionCap)
}
