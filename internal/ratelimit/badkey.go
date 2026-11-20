package ratelimit

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/automoto/gg-scale/internal/tenant"
)

// BadKeyRate / BadKeyBurst bound unknown API keys per client IP: 60 per
// minute, burst 60. That leaves room for a shared NAT with a misconfigured
// client, and holds one source to about one key lookup a second per instance.
const (
	BadKeyRate  = 1.0
	BadKeyBurst = 60.0
)

type badKeyLimiter struct {
	lim       Limiter
	refund    Refunder
	trust     *ProxyTrust
	throttled *prometheus.CounterVec
}

// NewBadKeyLimiter returns the per-IP unknown-key limit for
// tenant.NewLimited. Only unknown keys spend the budget, so valid keys are
// never charged for other clients' failures. The limit is checked only on a
// cache miss: an IP over the budget gets a 429 (error
// "too_many_unknown_keys") for keys the cache does not hold, and keys it
// holds keep working. Returns nil when lim cannot refund: Allow alone would
// spend budget on valid keys.
func NewBadKeyLimiter(lim Limiter, trust *ProxyTrust, reg prometheus.Registerer) tenant.FailureLimiter {
	rf, ok := lim.(Refunder)
	if !ok {
		return nil
	}
	return &badKeyLimiter{lim: lim, refund: rf, trust: trust, throttled: ipThrottledCounter(reg)}
}

// Allow checks the bucket without spending it: it takes a token and gives it
// back at once.
func (b *badKeyLimiter) Allow(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	bucket := ipBucketKey(b.trust.ClientIP(r), "badkey")
	decision, err := b.lim.Allow(ctx, bucket, BadKeyRate, BadKeyBurst)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if !decision.Allowed {
		writeRateLimitedCode(w, decision, "too_many_unknown_keys")
		b.throttled.WithLabelValues("badkey").Inc()
		return false
	}
	if err := b.refund.Refund(ctx, bucket, BadKeyRate, BadKeyBurst); err != nil {
		slog.WarnContext(ctx, "ratelimit: bad-key bucket refund", "err", err)
	}
	return true
}

// Fail spends one token for an unknown key.
func (b *badKeyLimiter) Fail(r *http.Request) {
	ctx := r.Context()
	bucket := ipBucketKey(b.trust.ClientIP(r), "badkey")
	if _, err := b.lim.Allow(ctx, bucket, BadKeyRate, BadKeyBurst); err != nil {
		slog.WarnContext(ctx, "ratelimit: bad-key bucket", "err", err)
	}
}
