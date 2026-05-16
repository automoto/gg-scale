package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
)

// EndUserRate / EndUserBurst are the defaults for the per-end-user
// limiter mounted on authenticated routes. 30 requests/sec with a burst
// of 60 is the rough budget a single player can sustain for storage,
// leaderboard, matchmaker, and relay calls. Operators tune via env.
const (
	EndUserRate  = 30.0
	EndUserBurst = 60.0
)

// NewEndUserLimiter buckets requests per (tenant, end_user_id). It sits
// alongside the per-api-key limiter so one abusive player can't drain the
// shared api-key bucket for every other player on the same key.
//
// Mount inside the end-user-authenticated subgroup (after enduser.New
// installs the id on the request context). When no end-user is in
// context the middleware is a no-op — the upstream tenant + api-key
// limiters still apply.
func NewEndUserLimiter(lim Limiter, ratePerSecond, burst float64, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_enduser_throttled_total",
			Help: "Authenticated requests throttled by the per-end-user limiter.",
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
			endUserID, ok := enduser.IDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			tenantID, _ := db.TenantFromContext(r.Context())
			bucket := endUserBucketKey(tenantID, endUserID)
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
				throttled.WithLabelValues("enduser").Inc()
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func endUserBucketKey(tenantID, endUserID int64) string {
	return fmt.Sprintf("ratelimit:enduser:%d:%d", tenantID, endUserID)
}
