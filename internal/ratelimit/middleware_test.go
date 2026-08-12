package ratelimit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

type fakeLimiter struct {
	decision ratelimit.Decision
	err      error
	calls    int
	keys     []string
	rates    []float64
	bursts   []float64
}

func (f *fakeLimiter) Allow(_ context.Context, key string, rate, burst float64) (ratelimit.Decision, error) {
	f.calls++
	f.keys = append(f.keys, key)
	f.rates = append(f.rates, rate)
	f.bursts = append(f.bursts, burst)
	return f.decision, f.err
}

func nopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func reqWithKey(key tenant.APIKey) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	return r.WithContext(tenant.WithAPIKey(r.Context(), key))
}

func TestMiddleware_passes_through_when_decision_allowed(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.Tier0})
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, lim.calls)
}

func TestMiddleware_returns_429_with_retry_after_and_json_body_when_denied(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 250 * time.Millisecond}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.Tier0})
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "1", rr.Header().Get("Retry-After"), "Retry-After is in seconds, rounded up")
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	body, err := io.ReadAll(rr.Body)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed))
	assert.Equal(t, "rate_limit_exceeded", parsed["error"])
	assert.NotNil(t, parsed["retry_after_seconds"])
}

func TestMiddleware_increments_throttled_counter_on_denial(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 100 * time.Millisecond}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.Tier0})
		mw(nopHandler()).ServeHTTP(rr, req)
	}

	mfs, err := reg.Gather()
	require.NoError(t, err)
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "ggscale_ratelimit_throttled_total" {
			found = true
			require.Len(t, mf.GetMetric(), 1)
			assert.Equal(t, float64(3), mf.GetMetric()[0].GetCounter().GetValue())
		}
	}
	assert.True(t, found, "ggscale_ratelimit_throttled_total must be registered")
}

func TestMiddleware_returns_500_on_limiter_error(t *testing.T) {
	lim := &fakeLimiter{err: errors.New("redis down")}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.Tier0})
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestMiddleware_returns_500_when_no_api_key_in_context(t *testing.T) {
	lim := &fakeLimiter{}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Equal(t, 0, lim.calls, "limiter not called without an api key")
}

func TestMiddleware_keys_bucket_by_api_key_id(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, nil, reg)

	for _, id := range []int64{42, 43, 42} {
		rr := httptest.NewRecorder()
		req := reqWithKey(tenant.APIKey{ID: id, TenantID: 5, Tier: tenant.Tier0})
		mw(nopHandler()).ServeHTTP(rr, req)
	}

	require.Len(t, lim.keys, 3)
	assert.True(t, strings.Contains(lim.keys[0], "42"))
	assert.True(t, strings.Contains(lim.keys[1], "43"))
	assert.True(t, strings.Contains(lim.keys[2], "42"))
	assert.NotEqual(t, lim.keys[0], lim.keys[1])
	assert.Equal(t, lim.keys[0], lim.keys[2])
}

func TestMiddleware_applies_tenant_override_independently_to_each_api_key(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	overrides := &countingOverrides{
		apiLimit: ratelimit.Limits{RatePerSecond: 25_000, Burst: 250_000},
		apiFound: true,
	}
	mw := ratelimit.New(lim, overrides, prometheus.NewRegistry())

	for _, id := range []int64{42, 43} {
		rr := httptest.NewRecorder()
		req := reqWithKey(tenant.APIKey{ID: id, TenantID: 5, Tier: tenant.Tier2})
		mw(nopHandler()).ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
	}

	assert.Equal(t, []float64{25_000, 25_000}, lim.rates)
	assert.Equal(t, []float64{250_000, 250_000}, lim.bursts)
	require.Len(t, lim.keys, 2)
	assert.NotEqual(t, lim.keys[0], lim.keys[1], "each API key receives an independent custom bucket")
}

func TestTierLimits_ladder_values_per_class(t *testing.T) {
	cases := []struct {
		tier      tenant.Tier
		wantRate  float64
		wantBurst float64
	}{
		{tenant.Tier0, 250, 2500},
		{tenant.Tier1, 1000, 10000},
		{tenant.Tier2, 5000, 50000},
		{tenant.Tier3, 10000, 100000},
	}
	for _, tc := range cases {
		got := ratelimit.LimitsForTier(tc.tier)
		assert.Equal(t, tc.wantRate, got.RatePerSecond, "tier=%s rate", tc.tier)
		assert.Equal(t, tc.wantBurst, got.Burst, "tier=%s burst", tc.tier)
	}
}

func TestTierLimits_burst_covers_one_request_per_sustained_connection(t *testing.T) {
	for _, tier := range []tenant.Tier{tenant.Tier0, tenant.Tier1, tenant.Tier2, tenant.Tier3} {
		got := ratelimit.LimitsForTier(tier)
		connections := ratelimit.ConnectionCapForClass(tier)
		assert.Equal(t, float64(connections.Sustained), got.Burst, "tier=%s", tier)
	}
}

func TestTierLimits_unknown_class_falls_back_to_tier0(t *testing.T) {
	got := ratelimit.LimitsForTier(tenant.Tier(99))
	t0 := ratelimit.LimitsForTier(tenant.Tier0)

	assert.Equal(t, t0, got)
}
