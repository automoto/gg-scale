// Package ratelimit applies a per-API-key token bucket to /v1/* requests.
//
// Tier limits are static defaults from LimitsForTier; the bucket itself
// lives in Valkey (see ValkeyLimiter) so it survives ggscale-server
// restarts and is shared across replicas. The middleware is meant to be
// mounted after internal/tenant.Middleware which installs the APIKey
// in the request context.
//
// Failure modes:
//   - 429 Too Many Requests: bucket empty; Retry-After (seconds, rounded
//     up) and a JSON body { "error": "rate_limit_exceeded", "retry_after_seconds": N }
//   - 500 Internal Server Error: limiter backend error or missing API key
//     context (a wiring bug — fail closed)
package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/tenant"
)

// Decision is the outcome of a single bucket check.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// Limiter takes a key and the tier's bucket parameters and returns a
// Decision. Implementations must be safe for concurrent use.
type Limiter interface {
	Allow(ctx context.Context, key string, ratePerSecond, burst float64) (Decision, error)
}

const routeClassDefault = "v1"

// New builds the rate-limit middleware.
//
// The throttled counter (ggscale_ratelimit_throttled_total{tier,route_class})
// is registered on reg so callers control the registry — useful for tests
// and for keeping the production registry isolated from package globals.
func New(lim Limiter, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_throttled_total",
			Help: "HTTP requests throttled by the rate-limit middleware.",
		},
		[]string{"tier", "route_class"},
	)
	reg.MustRegister(throttled)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			limits := LimitsForTier(key.Tier)
			bucket := bucketKey(key.ID, routeClassDefault)
			decision, err := lim.Allow(r.Context(), bucket, limits.RatePerSecond, limits.Burst)
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
				throttled.WithLabelValues(string(key.Tier), routeClassDefault).Inc()
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func bucketKey(apiKeyID int64, routeClass string) string {
	return fmt.Sprintf("ratelimit:%d:%s", apiKeyID, routeClass)
}
