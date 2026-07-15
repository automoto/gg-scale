// Package ratelimit applies a per-API-key token bucket to /v1/* requests.
//
// Tier limits are static defaults from LimitsForTier; the bucket itself
// lives in cache.Store (see CacheLimiter). With CACHE_BACKEND=memory the
// bucket is per-process; with CACHE_BACKEND=olric it is shared across
// the regional cluster of app processes. The middleware is meant to be
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
	"log/slog"
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

// Refunder credits a previously-debited token back to a bucket, capped at the
// bucket's burst. It is optional: callers type-assert a Limiter to Refunder and
// skip the refund when unsupported. Used to undo a debit when the guarded
// action fails after the token was taken.
type Refunder interface {
	Refund(ctx context.Context, key string, ratePerSecond, burst float64) error
}

const routeClassDefault = "v1"

// APIKeyBucketKey returns the default route-class bucket key for apiKeyID.
// ControlPanel revocation uses this to clear limiter state immediately.
func APIKeyBucketKey(apiKeyID int64) string {
	return bucketKey(apiKeyID, routeClassDefault)
}

// New builds the rate-limit middleware.
//
// overrides (may be nil) is consulted per request for a tenant-level API
// limit; when none is set, the compiled tier default from LimitsForTier
// applies. Wrap a DBOverrideStore in a CachedOverrideStore so this stays
// off the hot path — an override-lookup error is non-fatal and falls back
// to the tier default rather than failing the request.
//
// The throttled counter (ggscale_ratelimit_throttled_total{tier,route_class})
// is registered on reg so callers control the registry — useful for tests
// and for keeping the production registry isolated from package globals.
func New(lim Limiter, overrides OverrideStore, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_throttled_total",
			Help: "HTTP requests throttled by the rate-limit middleware.",
		},
		[]string{"tier", "route_class"},
	)
	if err := reg.Register(throttled); err != nil {
		are, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		throttled = are.ExistingCollector.(*prometheus.CounterVec)
	}
	overrideErrs := newOverrideErrorCounter(reg)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			limits := LimitsForTier(key.Tier)
			if overrides != nil {
				o, ok, oerr := overrides.APILimit(r.Context(), key.TenantID)
				switch {
				case oerr != nil:
					overrideErrs.WithLabelValues("api").Inc()
					slog.WarnContext(r.Context(), "rate-limit override lookup failed; using tier default",
						"err", oerr, "tenant_id", key.TenantID)
				case ok:
					limits = o
				}
			}
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
				throttled.WithLabelValues(key.Tier.String(), routeClassDefault).Inc()
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func bucketKey(apiKeyID int64, routeClass string) string {
	return fmt.Sprintf("ratelimit:%d:%s", apiKeyID, routeClass)
}
