package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/tenant"
)

// ProxyTrust resolves the real client IP for per-IP limiting when the server
// sits behind a reverse proxy / load balancer. Behind an LB, r.RemoteAddr is
// the proxy's address on every request, so a naive per-IP limiter collapses
// into a single global bucket. The forwarded header is honored ONLY when the
// TCP peer is inside a trusted CIDR, so a direct client can't spoof it.
type ProxyTrust struct {
	header   string
	networks []*net.IPNet
}

// NewProxyTrust builds a ProxyTrust from a header name (e.g. "CF-Connecting-IP")
// and trusted-proxy CIDRs. Returns nil when either is empty — the limiter then
// keys on the raw TCP peer, which is correct for a direct-to-internet deploy.
func NewProxyTrust(header string, cidrs []string) *ProxyTrust {
	if header == "" || len(cidrs) == 0 {
		return nil
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	if len(nets) == 0 {
		return nil
	}
	return &ProxyTrust{header: header, networks: nets}
}

// ClientIP returns the effective client IP: the forwarded header's real client
// when the TCP peer is a trusted proxy, otherwise the peer address. Safe to
// call on a nil ProxyTrust (returns the raw peer).
//
// Trusted proxies append the peer they received from on the right of a
// forwarded list, so the real client is the rightmost value NOT in a trusted
// CIDR. Walking right-to-left and discarding trusted hops means a client can't
// forge its bucket by prepending a value — anything it sends sits to the left
// of the trusted edge and is skipped.
func (p *ProxyTrust) ClientIP(r *http.Request) string {
	host := clientIP(r.RemoteAddr)
	if p == nil {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipInAnyNet(peer, p.networks) {
		return host
	}
	parts := strings.Split(r.Header.Get(p.header), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil || ipInAnyNet(ip, p.networks) {
			continue // malformed, or another trusted hop
		}
		return ip.String()
	}
	return host
}

func ipInAnyNet(ip net.IP, networks []*net.IPNet) bool {
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// AuthIPRate / AuthIPBurst are the defaults the router mounts on the
// bcrypt-heavy auth endpoints (signup, login): 10 requests / minute /
// IP, burst 10. Tuned to fit a legitimate "signup → login + a couple of
// retries" flow in one minute, but still cap a bcrypt-fishing attacker
// from a single source to ~2.5s of CPU per minute (bcryptCost=12).
const (
	AuthIPRate  = 10.0 / 60.0
	AuthIPBurst = 10.0
)

// NewIPLimiter buckets per source IP, *not* per api_key. It exists so
// the bcrypt endpoints (signup, login — ~250ms of CPU per attempt) can
// be capped by RemoteAddr even when the attacker holds a valid api_key.
// Mount on the auth subgroup, after the tenant middleware but
// independently of the api-key-keyed New() limiter.
//
// Trust model: the client IP comes from trust.ClientIP(r) — the raw TCP peer
// for a direct deployment, or the forwarded header when the peer is a trusted
// proxy (see ProxyTrust). Pass a nil trust for RemoteAddr-only behavior.
// Behind a load balancer a nil/misconfigured trust makes every request share
// the proxy's IP bucket, so production deployments MUST supply a ProxyTrust.
func NewIPLimiter(lim Limiter, ratePerSecond, burst float64, trust *ProxyTrust, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := ipThrottledCounter(reg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := trust.ClientIP(r)
			bucket := ipBucketKey(ip, "auth")
			decision, err := lim.Allow(r.Context(), bucket, ratePerSecond, burst)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !decision.Allowed {
				writeRateLimited(w, decision)
				throttled.WithLabelValues("auth").Inc()
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TokenRouteIPDivisor scales a tier's per-key limits down to the per-IP
// ceiling on the token auth endpoints (anonymous, verify, refresh,
// logout, custom-token): each publishable-key source IP gets 1/10 of
// the tenant's tier rate. Sized so the worst legitimate single-IP
// pattern (a tenant's whole CCU cap behind one NAT refreshing
// 15-minute tokens) keeps ~9× headroom, while a single source cannot
// fill the player quota or drain the per-key bucket at full tier speed.
const TokenRouteIPDivisor = 10

// TokenIPLimitsForTier returns the per-IP token-route bucket for a
// tier: the tier's per-key limits divided by TokenRouteIPDivisor.
func TokenIPLimitsForTier(t tenant.Tier) Limits {
	l := LimitsForTier(t)
	return Limits{
		RatePerSecond: l.RatePerSecond / TokenRouteIPDivisor,
		Burst:         l.Burst / TokenRouteIPDivisor,
	}
}

// NewTokenIPLimiter buckets per (tenant, source IP) with tier-scaled
// limits. It guards the non-bcrypt auth endpoints, whose fixed per-IP
// cap was removed to honor advertised tier rates: publishable keys ship
// inside game binaries, so one source holding a key must not be able to
// burn a tenant's player quota or per-key bucket alone. Secret keys
// pass through untouched — they identify the tenant's own backend,
// which legitimately concentrates traffic on few IPs (server-side
// custom-token exchange, load harnesses) and stays accountable to the
// per-key tier bucket.
//
// limitsFor overrides the tier→limits derivation; nil means
// TokenIPLimitsForTier. Mount after the tenant middleware — a missing
// API key in context is a wiring bug and fails closed with 500. The
// ProxyTrust caveats on NewIPLimiter apply here too.
func NewTokenIPLimiter(lim Limiter, limitsFor func(tenant.Tier) Limits, trust *ProxyTrust, reg prometheus.Registerer) func(http.Handler) http.Handler {
	if limitsFor == nil {
		limitsFor = TokenIPLimitsForTier
	}
	throttled := ipThrottledCounter(reg)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if key.Type == tenant.KeyTypeSecret {
				next.ServeHTTP(w, r)
				return
			}
			limits := limitsFor(key.Tier)
			bucket := tenantIPBucketKey(key.TenantID, trust.ClientIP(r), "token")
			decision, err := lim.Allow(r.Context(), bucket, limits.RatePerSecond, limits.Burst)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !decision.Allowed {
				writeRateLimited(w, decision)
				throttled.WithLabelValues("token").Inc()
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ipThrottledCounter registers (or reuses) the shared per-IP throttle
// counter on reg; both IP limiters report here under their route class.
func ipThrottledCounter(reg prometheus.Registerer) *prometheus.CounterVec {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_ip_throttled_total",
			Help: "Auth requests throttled by the per-IP rate limiters.",
		},
		[]string{"route_class"},
	)
	if err := reg.Register(throttled); err != nil {
		are, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		throttled = are.ExistingCollector.(*prometheus.CounterVec)
	}
	return throttled
}

// clientIP returns the host portion of remoteAddr. Falls back to the
// raw value when SplitHostPort fails (rare — net/http always sets a
// host:port pair, but be defensive).
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func ipBucketKey(ip, routeClass string) string {
	return fmt.Sprintf("ratelimit:ip:%s:%s", routeClass, ip)
}

// tenantIPBucketKey scopes an IP bucket to a tenant so shared NAT/CGNAT
// addresses never cross-contaminate tenants (and tier-dependent limits
// stay coherent on one bucket).
func tenantIPBucketKey(tenantID int64, ip, routeClass string) string {
	return fmt.Sprintf("ratelimit:ip:%s:%d:%s", routeClass, tenantID, ip)
}
