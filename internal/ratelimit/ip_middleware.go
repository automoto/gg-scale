package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

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
// Trust model: only r.RemoteAddr (the TCP peer) is consulted —
// X-Forwarded-For is intentionally ignored to avoid spoof bypass. Once
// ggscale-server runs behind a trusted load balancer, replace this with
// a proxy-aware extractor gated on a TrustedProxies allowlist.
func NewIPLimiter(lim Limiter, ratePerSecond, burst float64, reg prometheus.Registerer) func(http.Handler) http.Handler {
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
			ip := clientIP(r.RemoteAddr)
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
