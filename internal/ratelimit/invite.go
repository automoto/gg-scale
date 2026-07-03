package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Invite-throttle scope labels for the throttled metric.
const (
	inviteScopeInviter   = "inviter"
	inviteScopeDomain    = "domain"
	inviteScopeRecipient = "recipient"
)

// InviteLimits parameterises the three invite-send buckets. Defaults are
// deliberately generous for a legitimate admin while capping bulk abuse.
type InviteLimits struct {
	// InviterPerHour caps invites a single dashboard user may send per hour.
	InviterPerHour float64
	// DomainPerDay caps invites per project (player invites) or per tenant
	// (team invites) per day.
	DomainPerDay float64
	// RecipientCooldown is the minimum gap between invites to the same
	// address within a scope.
	RecipientCooldown time.Duration
}

// DefaultInviteLimits are the built-in invite caps.
var DefaultInviteLimits = InviteLimits{
	InviterPerHour:    10,
	DomainPerDay:      100,
	RecipientCooldown: 10 * time.Minute,
}

// InviteThrottle applies per-inviter, per-domain, and per-recipient token
// buckets to invite creation. It is backed by the same cache.Store token
// bucket as the HTTP limiter, so state is shared across a clustered
// deployment.
type InviteThrottle struct {
	lim          Limiter
	limits       InviteLimits
	overrides    OverrideStore
	throttled    *prometheus.CounterVec
	overrideErrs *prometheus.CounterVec
}

// InviteAttempt identifies one invite send for throttling.
type InviteAttempt struct {
	// InviterID is the dashboard user sending the invite.
	InviterID int64
	// TenantID and ProjectID scope override lookups. ProjectID is 0 for team
	// invites (tenant-wide overrides only).
	TenantID  int64
	ProjectID int64
	// DomainKey scopes the daily cap and the recipient cooldown, e.g.
	// "project:7" for player invites or "tenant:3" for team invites.
	DomainKey string
	// Recipient is the normalized invitee email.
	Recipient string
}

// NewInviteThrottle builds an invite throttle. The
// ggscale_ratelimit_invite_throttled_total{scope} counter is registered on
// reg (idempotently) so callers own the registry.
func NewInviteThrottle(lim Limiter, limits InviteLimits, reg prometheus.Registerer) *InviteThrottle {
	throttled := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ggscale_ratelimit_invite_throttled_total",
			Help: "Invite sends throttled, by which bucket rejected.",
		},
		[]string{"scope"},
	)
	if err := reg.Register(throttled); err != nil {
		are, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		throttled = are.ExistingCollector.(*prometheus.CounterVec)
	}
	return &InviteThrottle{
		lim:          lim,
		limits:       limits,
		throttled:    throttled,
		overrideErrs: newOverrideErrorCounter(reg),
	}
}

// WithOverrides returns a copy that consults store for per-tenant/project
// invite-limit overrides before falling back to the built-in defaults.
func (t *InviteThrottle) WithOverrides(store OverrideStore) *InviteThrottle {
	if t == nil {
		return nil
	}
	cp := *t
	cp.overrides = store
	return &cp
}

// inviteBucket is one token bucket the throttle checks, with its
// override-resolved rate/burst already applied.
type inviteBucket struct {
	scope      string
	key        string
	ratePerSec float64
	burst      float64
}

// buckets resolves the three invite buckets for an attempt, applying any
// per-tenant/project overrides. On an override-store error it logs, counts the
// error, and keeps the compiled default for that bucket.
//
// The inviter and domain keys are scoped by DomainKey (project:<id> for player
// invites, tenant:<id> for team invites) so a bucket's token state shares the
// same granularity as the override that sets its rate/burst — otherwise a
// project-scoped override would reshape a globally-shared bucket.
func (t *InviteThrottle) buckets(ctx context.Context, a InviteAttempt) []inviteBucket {
	recipientBurst := 0.0
	if t.limits.RecipientCooldown > 0 {
		recipientBurst = 1
	}
	specs := []struct {
		scope      string
		key        string
		kind       string
		ratePerSec float64
		burst      float64
	}{
		{
			scope:      inviteScopeRecipient,
			key:        fmt.Sprintf("invite:recipient:%s:%s", a.DomainKey, hashEmail(a.Recipient)),
			kind:       OverrideKindInviteRecipient,
			ratePerSec: ratePerSecond(1, t.limits.RecipientCooldown),
			burst:      recipientBurst,
		},
		{
			scope:      inviteScopeInviter,
			key:        fmt.Sprintf("invite:inviter:%s:%d", a.DomainKey, a.InviterID),
			kind:       OverrideKindInviteInviter,
			ratePerSec: t.limits.InviterPerHour / 3600,
			burst:      t.limits.InviterPerHour,
		},
		{
			scope:      inviteScopeDomain,
			key:        "invite:domain:" + a.DomainKey,
			kind:       OverrideKindInviteDomain,
			ratePerSec: t.limits.DomainPerDay / 86400,
			burst:      t.limits.DomainPerDay,
		},
	}

	out := make([]inviteBucket, len(specs))
	for i, s := range specs {
		rate, burst := s.ratePerSec, s.burst
		if t.overrides != nil {
			o, ok, err := t.overrides.InviteLimit(ctx, a.TenantID, a.ProjectID, s.kind)
			switch {
			case err != nil:
				t.overrideErrs.WithLabelValues("invite").Inc()
				slog.WarnContext(ctx, "invite override lookup failed; using default",
					"err", err, "tenant_id", a.TenantID, "project_id", a.ProjectID, "kind", s.kind)
			case ok:
				rate, burst = o.RatePerSecond, o.Burst
			}
		}
		out[i] = inviteBucket{scope: s.scope, key: s.key, ratePerSec: rate, burst: burst}
	}
	return out
}

// Check debits one token from each relevant bucket. It returns a Decision;
// when Allowed is false, RetryAfter is the wait for the bucket that rejected.
// A nil throttle (feature unwired) always allows.
//
// Tokens are debited before the invite is created; Refund undoes the debit when
// the create fails so a duplicate/transient error doesn't burn a real invite's
// quota.
func (t *InviteThrottle) Check(ctx context.Context, a InviteAttempt) (Decision, error) {
	if t == nil || t.lim == nil {
		return Decision{Allowed: true}, nil
	}
	for _, b := range t.buckets(ctx, a) {
		if b.burst <= 0 {
			continue // a zero-burst limit is "unlimited" for this bucket
		}
		decision, err := t.lim.Allow(ctx, b.key, b.ratePerSec, b.burst)
		if err != nil {
			return Decision{}, err
		}
		if !decision.Allowed {
			t.throttled.WithLabelValues(b.scope).Inc()
			return decision, nil
		}
	}
	return Decision{Allowed: true}, nil
}

// Refund credits back the tokens a successful Check debited. Call it only after
// a Check that returned Allowed=true when the subsequent invite create fails,
// so a rejected invite doesn't permanently consume quota. It is best-effort: a
// no-op when the limiter can't refund, and refund errors are swallowed (the
// bucket self-heals as it refills). A nil throttle is a no-op.
func (t *InviteThrottle) Refund(ctx context.Context, a InviteAttempt) {
	if t == nil || t.lim == nil {
		return
	}
	refunder, ok := t.lim.(Refunder)
	if !ok {
		return
	}
	for _, b := range t.buckets(ctx, a) {
		if b.burst <= 0 {
			continue
		}
		if err := refunder.Refund(ctx, b.key, b.ratePerSec, b.burst); err != nil {
			slog.WarnContext(ctx, "invite token refund failed", "err", err, "scope", b.scope)
		}
	}
}

func ratePerSecond(count float64, per time.Duration) float64 {
	if per <= 0 {
		return 0
	}
	return count / per.Seconds()
}

// hashEmail keeps recipient addresses out of cache keys (PII) while still
// giving each address a stable bucket.
func hashEmail(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:8])
}
