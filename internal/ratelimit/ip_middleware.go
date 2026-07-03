package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
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

// AuthIPRate / AuthIPBurst are the defaults the router mounts on
// /v1/auth/*: 10 requests / minute / IP, burst 10. Tuned to fit a
// legitimate "signup → verify → login → refresh + a couple of retries"
// flow in one minute, but still cap a bcrypt-fishing attacker from a
// single source to ~2.5s of CPU per minute (bcryptCost=12).
const (
	AuthIPRate  = 10.0 / 60.0
	AuthIPBurst = 10.0
)

// NewIPLimiter buckets per source IP, *not* per api_key. It exists so
// /v1/auth/* (which runs bcrypt on every login attempt — ~250ms of CPU)
// can be capped by RemoteAddr even when the attacker holds a valid
// api_key. Mount on the auth subgroup, after the tenant middleware but
// independently of the api-key-keyed New() limiter.
//
// Trust model: the client IP comes from trust.ClientIP(r) — the raw TCP peer
// for a direct deployment, or the forwarded header when the peer is a trusted
// proxy (see ProxyTrust). Pass a nil trust for RemoteAddr-only behavior.
// Behind a load balancer a nil/misconfigured trust makes every request share
// the proxy's IP bucket, so production deployments MUST supply a ProxyTrust.
func NewIPLimiter(lim Limiter, ratePerSecond, burst float64, trust *ProxyTrust, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_ip_throttled_total",
			Help: "Auth requests throttled by the per-IP rate limiter.",
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
				retrySec := int(math.Ceil(decision.RetryAfter.Seconds()))
				if retrySec < 1 {
					retrySec = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":               "rate_limit_exceeded",
					"retry_after_seconds": retrySec,
				})
				throttled.WithLabelValues("auth").Inc()
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
