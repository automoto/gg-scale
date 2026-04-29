package ratelimit_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

func ipReq(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	r.RemoteAddr = remoteAddr
	return r
}

func TestIPLimiter_passes_through_when_allowed(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, lim.calls)
}

func TestIPLimiter_returns_429_with_retry_after_when_denied(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 30 * time.Second}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "30", rr.Header().Get("Retry-After"))
}

func TestIPLimiter_keys_bucket_by_client_ip_stripping_port(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	for _, addr := range []string{
		"203.0.113.7:50000",
		"203.0.113.8:51111",
		"203.0.113.7:60000",
	} {
		rr := httptest.NewRecorder()
		mw(nopHandler()).ServeHTTP(rr, ipReq(addr))
	}

	require.Len(t, lim.keys, 3)
	assert.True(t, strings.Contains(lim.keys[0], "203.0.113.7"))
	assert.True(t, strings.Contains(lim.keys[1], "203.0.113.8"))
	assert.NotEqual(t, lim.keys[0], lim.keys[1])
	assert.Equal(t, lim.keys[0], lim.keys[2], "same IP, different ephemeral port → same bucket")
}

func TestIPLimiter_does_not_trust_x_forwarded_for_by_default(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	r := ipReq("203.0.113.7:50000")
	r.Header.Set("X-Forwarded-For", "198.51.100.99")
	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, r)

	require.Len(t, lim.keys, 1)
	assert.Contains(t, lim.keys[0], "203.0.113.7")
	assert.NotContains(t, lim.keys[0], "198.51.100.99")
}

func TestIPLimiter_handles_ipv6_remote_addr(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("[2001:db8::1]:50000"))

	require.Len(t, lim.keys, 1)
	assert.Contains(t, lim.keys[0], "2001:db8::1")
}

func TestIPLimiter_returns_500_on_limiter_error(t *testing.T) {
	lim := &fakeLimiter{err: errors.New("redis down")}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}
