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

	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

type fakeLimiter struct {
	decision ratelimit.Decision
	err      error
	calls    int
	keys     []string
}

func (f *fakeLimiter) Allow(_ context.Context, key string, _, _ float64) (ratelimit.Decision, error) {
	f.calls++
	f.keys = append(f.keys, key)
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
	mw := ratelimit.New(lim, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.TierFree})
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, lim.calls)
}

func TestMiddleware_returns_429_with_retry_after_and_json_body_when_denied(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 250 * time.Millisecond}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.TierFree})
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
	mw := ratelimit.New(lim, reg)

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.TierFree})
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
	mw := ratelimit.New(lim, reg)

	rr := httptest.NewRecorder()
	req := reqWithKey(tenant.APIKey{ID: 1, TenantID: 5, Tier: tenant.TierFree})
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestMiddleware_returns_500_when_no_api_key_in_context(t *testing.T) {
	lim := &fakeLimiter{}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, reg)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Equal(t, 0, lim.calls, "limiter not called without an api key")
}

func TestMiddleware_keys_bucket_by_api_key_id(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.New(lim, reg)

	for _, id := range []int64{42, 43, 42} {
		rr := httptest.NewRecorder()
		req := reqWithKey(tenant.APIKey{ID: id, TenantID: 5, Tier: tenant.TierFree})
		mw(nopHandler()).ServeHTTP(rr, req)
	}

	require.Len(t, lim.keys, 3)
	assert.True(t, strings.Contains(lim.keys[0], "42"))
	assert.True(t, strings.Contains(lim.keys[1], "43"))
	assert.True(t, strings.Contains(lim.keys[2], "42"))
	assert.NotEqual(t, lim.keys[0], lim.keys[1])
	assert.Equal(t, lim.keys[0], lim.keys[2])
}

func TestTierLimits_known_tiers_have_distinct_rates(t *testing.T) {
	free := ratelimit.LimitsForTier(tenant.TierFree)
	payg := ratelimit.LimitsForTier(tenant.TierPAYG)
	premium := ratelimit.LimitsForTier(tenant.TierPremium)

	assert.Equal(t, float64(60), free.RatePerSecond)
	assert.Equal(t, float64(600), payg.RatePerSecond)
	assert.Greater(t, premium.RatePerSecond, payg.RatePerSecond)
	assert.Greater(t, free.Burst, float64(0))
}

func TestTierLimits_unknown_tier_falls_back_to_free(t *testing.T) {
	got := ratelimit.LimitsForTier("never-heard-of-it")
	free := ratelimit.LimitsForTier(tenant.TierFree)

	assert.Equal(t, free, got)
}
