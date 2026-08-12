package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

// captureLimiter records the bucket parameters each Allow call receives;
// the shared fakeLimiter only records keys.
type captureLimiter struct {
	decision ratelimit.Decision
	keys     []string
	rates    []float64
	bursts   []float64
}

func (c *captureLimiter) Allow(_ context.Context, key string, rate, burst float64) (ratelimit.Decision, error) {
	c.keys = append(c.keys, key)
	c.rates = append(c.rates, rate)
	c.bursts = append(c.bursts, burst)
	return c.decision, nil
}

func tokenReq(remoteAddr string, key tenant.APIKey) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/anonymous", nil)
	r.RemoteAddr = remoteAddr
	return r.WithContext(tenant.WithAPIKey(r.Context(), key))
}

func publishableKey(tenantID int64, tier tenant.Tier) tenant.APIKey {
	return tenant.APIKey{ID: 1, TenantID: tenantID, Tier: tier, Type: tenant.KeyTypePublishable}
}

func TestTokenIPLimitsForTier_should_keep_auth_abuse_bursts_independent_of_api_buckets(t *testing.T) {
	tests := []struct {
		tier  tenant.Tier
		rate  float64
		burst float64
	}{
		{tenant.Tier0, 25, 50},
		{tenant.Tier1, 100, 200},
		{tenant.Tier2, 250, 500},
		{tenant.Tier3, 1000, 2000},
		{tenant.Tier(99), 25, 50},
	}
	for _, tc := range tests {
		got := ratelimit.TokenIPLimitsForTier(tc.tier)
		assert.Equal(t, tc.rate, got.RatePerSecond, "tier %v rate", tc.tier)
		assert.Equal(t, tc.burst, got.Burst, "tier %v burst", tc.tier)
	}
}

func TestTokenIPLimiter_should_derive_limits_from_tier(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: true}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, tokenReq("203.0.113.7:50000", publishableKey(7, tenant.Tier1)))

	assert.Equal(t, http.StatusOK, rr.Code)
	require.Len(t, lim.rates, 1)
	assert.Equal(t, 100.0, lim.rates[0])
	assert.Equal(t, 200.0, lim.bursts[0])
}

func TestTokenIPLimiter_should_use_custom_limits_when_provided(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: true}}
	custom := func(tenant.Tier) ratelimit.Limits {
		return ratelimit.Limits{RatePerSecond: 1, Burst: 3}
	}
	mw := ratelimit.NewTokenIPLimiter(lim, custom, nil, prometheus.NewRegistry())

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, tokenReq("203.0.113.7:50000", publishableKey(7, tenant.Tier2)))

	require.Len(t, lim.rates, 1)
	assert.Equal(t, 1.0, lim.rates[0])
	assert.Equal(t, 3.0, lim.bursts[0])
}

func TestTokenIPLimiter_should_bucket_by_tenant_and_ip(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: true}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())
	h := mw(nopHandler())

	h.ServeHTTP(httptest.NewRecorder(), tokenReq("203.0.113.7:50000", publishableKey(1, tenant.Tier0)))
	h.ServeHTTP(httptest.NewRecorder(), tokenReq("203.0.113.7:50001", publishableKey(2, tenant.Tier0)))
	h.ServeHTTP(httptest.NewRecorder(), tokenReq("203.0.113.8:50000", publishableKey(1, tenant.Tier0)))
	h.ServeHTTP(httptest.NewRecorder(), tokenReq("203.0.113.7:60000", publishableKey(1, tenant.Tier0)))

	require.Len(t, lim.keys, 4)
	assert.NotEqual(t, lim.keys[0], lim.keys[1], "same IP, different tenant → different bucket")
	assert.NotEqual(t, lim.keys[0], lim.keys[2], "same tenant, different IP → different bucket")
	assert.Equal(t, lim.keys[0], lim.keys[3], "same tenant and IP → same bucket")
}

func TestTokenIPLimiter_should_bypass_secret_keys(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: time.Minute}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())

	secret := tenant.APIKey{ID: 1, TenantID: 7, Tier: tenant.Tier0, Type: tenant.KeyTypeSecret}
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, tokenReq("203.0.113.7:50000", secret))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, lim.keys, "secret-key requests must not touch the limiter")
}

func TestTokenIPLimiter_should_limit_unknown_key_types(t *testing.T) {
	// The exemption must stay an exact match on KeyTypeSecret: an empty or
	// bogus type is limited, never exempt.
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: time.Minute}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())

	for _, typ := range []tenant.KeyType{"", "bogus"} {
		key := tenant.APIKey{ID: 1, TenantID: 7, Tier: tenant.Tier0, Type: typ}
		rr := httptest.NewRecorder()
		mw(nopHandler()).ServeHTTP(rr, tokenReq("203.0.113.7:50000", key))
		assert.Equalf(t, http.StatusTooManyRequests, rr.Code, "type %q must be limited, not exempt", typ)
	}
	assert.Len(t, lim.keys, 2)
}

func TestTokenIPLimiter_should_return_429_with_retry_after_when_denied(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 30 * time.Second}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, tokenReq("203.0.113.7:50000", publishableKey(7, tenant.Tier0)))

	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "30", rr.Header().Get("Retry-After"))
	assert.Contains(t, rr.Body.String(), "rate_limit_exceeded")
}

func TestTokenIPLimiter_should_fail_closed_without_key_context(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: true}}
	mw := ratelimit.NewTokenIPLimiter(lim, nil, nil, prometheus.NewRegistry())

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/anonymous", nil)
	req.RemoteAddr = "203.0.113.7:50000"
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Empty(t, lim.keys)
}

func TestTokenIPLimiter_should_use_forwarded_ip_behind_trusted_proxy(t *testing.T) {
	lim := &captureLimiter{decision: ratelimit.Decision{Allowed: true}}
	trust := ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"})
	mw := ratelimit.NewTokenIPLimiter(lim, nil, trust, prometheus.NewRegistry())

	req := tokenReq("10.0.0.1:44321", publishableKey(7, tenant.Tier0))
	req.Header.Set("CF-Connecting-IP", "198.51.100.9")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, req)

	require.Len(t, lim.keys, 1)
	assert.Contains(t, lim.keys[0], "198.51.100.9")
	assert.NotContains(t, lim.keys[0], "10.0.0.1")
}
