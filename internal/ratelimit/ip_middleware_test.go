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
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, lim.calls)
}

func TestIPLimiter_returns_429_with_retry_after_when_denied(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: false, RetryAfter: 30 * time.Second}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "30", rr.Header().Get("Retry-After"))
}

func TestIPLimiter_keys_bucket_by_client_ip_stripping_port(t *testing.T) {
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

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
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

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
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("[2001:db8::1]:50000"))

	require.Len(t, lim.keys, 1)
	assert.Contains(t, lim.keys[0], "2001:db8::1")
}

func TestIPLimiter_returns_500_on_limiter_error(t *testing.T) {
	lim := &fakeLimiter{err: errors.New("redis down")}
	reg := prometheus.NewRegistry()
	mw := ratelimit.NewIPLimiter(lim, 1, 5, nil, reg)

	rr := httptest.NewRecorder()
	mw(nopHandler()).ServeHTTP(rr, ipReq("203.0.113.7:50000"))

	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestProxyTrust_nil_returns_peer(t *testing.T) {
	var pt *ratelimit.ProxyTrust
	r := ipReq("203.0.113.7:50000")
	r.Header.Set("CF-Connecting-IP", "198.51.100.99")
	assert.Equal(t, "203.0.113.7", pt.ClientIP(r))
}

func TestProxyTrust_trusted_peer_uses_forwarded_header(t *testing.T) {
	pt := ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"})
	require.NotNil(t, pt)
	r := ipReq("10.1.2.3:40000") // peer is the trusted LB
	r.Header.Set("CF-Connecting-IP", "198.51.100.99")
	assert.Equal(t, "198.51.100.99", pt.ClientIP(r), "real client IP from the header")
}

func TestProxyTrust_untrusted_peer_ignores_header(t *testing.T) {
	// A direct client (not in the trusted CIDR) can't spoof the header.
	pt := ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"})
	r := ipReq("203.0.113.7:50000")
	r.Header.Set("CF-Connecting-IP", "10.0.0.5")
	assert.Equal(t, "203.0.113.7", pt.ClientIP(r))
}

func TestProxyTrust_xff_returns_rightmost_untrusted(t *testing.T) {
	// Proxies append the real peer on the right of X-Forwarded-For. The correct
	// client is the rightmost value NOT in a trusted CIDR: here the trusted LB
	// appended its own peer (10.1.2.3) after the real client (198.51.100.99).
	pt := ratelimit.NewProxyTrust("X-Forwarded-For", []string{"10.0.0.0/8"})
	r := ipReq("10.1.2.3:40000")
	r.Header.Set("X-Forwarded-For", "198.51.100.99, 10.1.2.3")
	assert.Equal(t, "198.51.100.99", pt.ClientIP(r))
}

func TestProxyTrust_xff_ignores_spoofed_leftmost(t *testing.T) {
	// The spoofing attack: an attacker prepends a forged value; the trusted
	// proxy appends the real client on the right. Keying on the leftmost value
	// would give the attacker a fresh bucket per request. Rightmost-untrusted
	// selects the real client instead.
	pt := ratelimit.NewProxyTrust("X-Forwarded-For", []string{"10.0.0.0/8"})
	r := ipReq("10.1.2.3:40000") // trusted LB peer
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 198.51.100.99")
	assert.Equal(t, "198.51.100.99", pt.ClientIP(r),
		"real client (rightmost untrusted), not the forged leftmost value")
}

func TestProxyTrust_xff_skips_multiple_trusted_hops(t *testing.T) {
	pt := ratelimit.NewProxyTrust("X-Forwarded-For", []string{"10.0.0.0/8"})
	r := ipReq("10.0.0.2:40000")
	r.Header.Set("X-Forwarded-For", "198.51.100.99, 10.0.0.1, 10.0.0.2")
	assert.Equal(t, "198.51.100.99", pt.ClientIP(r))
}

func TestProxyTrust_xff_all_trusted_falls_back_to_peer(t *testing.T) {
	pt := ratelimit.NewProxyTrust("X-Forwarded-For", []string{"10.0.0.0/8"})
	r := ipReq("10.0.0.2:40000")
	r.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	assert.Equal(t, "10.0.0.2", pt.ClientIP(r), "no untrusted hop → keep the peer")
}

func TestProxyTrust_single_value_header(t *testing.T) {
	// CF-Connecting-IP carries exactly one client IP; rightmost-untrusted on a
	// one-element list returns that value.
	pt := ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"})
	r := ipReq("10.1.2.3:40000")
	r.Header.Set("CF-Connecting-IP", "198.51.100.99")
	assert.Equal(t, "198.51.100.99", pt.ClientIP(r))
}

func TestNewProxyTrust_nil_when_unconfigured(t *testing.T) {
	assert.Nil(t, ratelimit.NewProxyTrust("", []string{"10.0.0.0/8"}))
	assert.Nil(t, ratelimit.NewProxyTrust("CF-Connecting-IP", nil))
	assert.Nil(t, ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"not-a-cidr"}))
}

func TestIPLimiter_behind_proxy_buckets_by_forwarded_ip(t *testing.T) {
	// The regression this fixes: behind a load balancer every request shares
	// the LB's RemoteAddr. With ProxyTrust, distinct real clients get distinct
	// buckets instead of collapsing into one global bucket.
	lim := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	pt := ratelimit.NewProxyTrust("CF-Connecting-IP", []string{"10.0.0.0/8"})
	mw := ratelimit.NewIPLimiter(lim, 1, 5, pt, prometheus.NewRegistry())

	for _, client := range []string{"198.51.100.1", "198.51.100.2"} {
		r := ipReq("10.1.2.3:40000") // same LB peer for both
		r.Header.Set("CF-Connecting-IP", client)
		mw(nopHandler()).ServeHTTP(httptest.NewRecorder(), r)
	}

	require.Len(t, lim.keys, 2)
	assert.Contains(t, lim.keys[0], "198.51.100.1")
	assert.Contains(t, lim.keys[1], "198.51.100.2")
	assert.NotEqual(t, lim.keys[0], lim.keys[1], "distinct clients → distinct buckets, not one LB bucket")
}
