package ratelimit

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/playerauth"
)

// PlayerRate / PlayerBurst are the defaults for the per-player
// limiter mounted on authenticated routes. 30 requests/sec with a burst
// of 60 is the rough budget a single player can sustain for storage,
// leaderboard, matchmaker, and relay calls. Operators tune via env.
const (
	PlayerRate  = 30.0
	PlayerBurst = 60.0
)

// NewPlayerLimiter buckets requests per (tenant, player_id). It sits
// alongside the per-api-key limiter so one abusive player can't drain the
// shared api-key bucket for every other player on the same key.
//
// Mount inside the player-authenticated subgroup (after playerauth.New
// installs the id on the request context). When no player is in
// context the middleware is a no-op — the upstream tenant + api-key
// limiters still apply.
func NewPlayerLimiter(lim Limiter, ratePerSecond, burst float64, reg prometheus.Registerer) func(http.Handler) http.Handler {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_player_throttled_total",
			Help: "Authenticated requests throttled by the per-player limiter.",
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
			playerID, ok := playerauth.IDFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			tenantID, _ := db.TenantFromContext(r.Context())
			bucket := playerBucketKey(tenantID, playerID)
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
				throttled.WithLabelValues("player").Inc()
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func playerBucketKey(tenantID, playerID int64) string {
	return fmt.Sprintf("ratelimit:player:%d:%d", tenantID, playerID)
}
