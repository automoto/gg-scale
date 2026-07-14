package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/ratelimit"
)

func newInviteThrottle(t *testing.T, limits ratelimit.InviteLimits) *ratelimit.InviteThrottle {
	t.Helper()
	lim := ratelimit.NewCacheLimiter(memory.New())
	return ratelimit.NewInviteThrottle(lim, limits, prometheus.NewRegistry())
}

func TestInviteThrottle_allows_within_limits(t *testing.T) {
	th := newInviteThrottle(t, ratelimit.DefaultInviteLimits)

	dec, err := th.Check(context.Background(), ratelimit.InviteAttempt{
		InviterID: 1, DomainKey: "project:7", Recipient: "a@example.com",
	})

	require.NoError(t, err)
	assert.True(t, dec.Allowed)
}

func TestInviteThrottle_blocks_repeat_to_same_recipient(t *testing.T) {
	th := newInviteThrottle(t, ratelimit.InviteLimits{
		InviterPerHour: 100, DomainPerDay: 100, RecipientCooldown: 10 * time.Minute,
	})
	attempt := ratelimit.InviteAttempt{InviterID: 1, DomainKey: "project:7", Recipient: "dup@example.com"}

	first, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	second, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.False(t, second.Allowed, "same recipient within cooldown must be blocked")
	assert.Positive(t, second.RetryAfter)
}

func TestInviteThrottle_allows_one_correction_to_same_recipient(t *testing.T) {
	// RecipientBurst 2 lets an admin fix a mistake: two back-to-back sends to
	// the same address are allowed, the third waits out the cooldown.
	th := newInviteThrottle(t, ratelimit.InviteLimits{
		InviterPerHour: 100, DomainPerDay: 100,
		RecipientCooldown: 10 * time.Minute, RecipientBurst: 2,
	})
	attempt := ratelimit.InviteAttempt{InviterID: 1, DomainKey: "project:7", Recipient: "fix@example.com"}

	first, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	second, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.True(t, second.Allowed, "a single correction to the same recipient is allowed")

	third, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.False(t, third.Allowed, "a third rapid send to the same recipient is blocked")
	assert.Positive(t, third.RetryAfter)
}

func TestInviteThrottle_different_recipients_not_blocked(t *testing.T) {
	th := newInviteThrottle(t, ratelimit.InviteLimits{
		InviterPerHour: 100, DomainPerDay: 100, RecipientCooldown: 10 * time.Minute,
	})

	a, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "project:7", Recipient: "one@example.com"})
	require.NoError(t, err)
	b, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "project:7", Recipient: "two@example.com"})
	require.NoError(t, err)

	assert.True(t, a.Allowed)
	assert.True(t, b.Allowed)
}

func TestInviteThrottle_enforces_per_inviter_hourly_cap(t *testing.T) {
	// 3 invites/hour, no recipient/domain limits interfering.
	th := newInviteThrottle(t, ratelimit.InviteLimits{InviterPerHour: 3})

	allowed := 0
	for i := 0; i < 5; i++ {
		dec, err := th.Check(context.Background(), ratelimit.InviteAttempt{
			InviterID: 42, DomainKey: "tenant:1", Recipient: uniqueEmail(i),
		})
		require.NoError(t, err)
		if dec.Allowed {
			allowed++
		}
	}
	assert.Equal(t, 3, allowed, "inviter capped at the hourly burst")
}

func TestInviteThrottle_per_inviter_is_isolated(t *testing.T) {
	th := newInviteThrottle(t, ratelimit.InviteLimits{InviterPerHour: 1})

	first, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "tenant:1", Recipient: "x@example.com"})
	require.NoError(t, err)
	require.True(t, first.Allowed)
	// Same inviter, second send → blocked.
	blocked, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "tenant:1", Recipient: "y@example.com"})
	require.NoError(t, err)
	require.False(t, blocked.Allowed)
	// A different inviter is unaffected.
	other, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 2, DomainKey: "tenant:1", Recipient: "z@example.com"})
	require.NoError(t, err)
	assert.True(t, other.Allowed)
}

type stubOverrides struct {
	invite map[string]ratelimit.Limits
}

func (s stubOverrides) APILimit(_ context.Context, _ int64) (ratelimit.Limits, bool, error) {
	return ratelimit.Limits{}, false, nil
}

func (s stubOverrides) InviteLimit(_ context.Context, _, _ int64, kind string) (ratelimit.Limits, bool, error) {
	l, ok := s.invite[kind]
	return l, ok, nil
}

func TestInviteThrottle_override_tightens_inviter_cap(t *testing.T) {
	// Default inviter cap is generous, but an override pins it to 1/hour.
	lim := ratelimit.NewCacheLimiter(memory.New())
	base := ratelimit.NewInviteThrottle(lim, ratelimit.DefaultInviteLimits, prometheus.NewRegistry())
	th := base.WithOverrides(stubOverrides{invite: map[string]ratelimit.Limits{
		ratelimit.OverrideKindInviteInviter: {RatePerSecond: 1.0 / 3600, Burst: 1},
	}})

	first, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 9, TenantID: 1, DomainKey: "tenant:1", Recipient: "a@example.com"})
	require.NoError(t, err)
	require.True(t, first.Allowed)

	second, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 9, TenantID: 1, DomainKey: "tenant:1", Recipient: "b@example.com"})
	require.NoError(t, err)
	assert.False(t, second.Allowed, "override cap of 1/hour blocks the second send")
}

func TestInviteThrottle_override_raises_recipient_burst(t *testing.T) {
	// A platform-admin override lifts the recipient burst from the default 2 to 3,
	// so three back-to-back sends to the same address pass and the fourth blocks.
	lim := ratelimit.NewCacheLimiter(memory.New())
	base := ratelimit.NewInviteThrottle(lim, ratelimit.DefaultInviteLimits, prometheus.NewRegistry())
	th := base.WithOverrides(stubOverrides{invite: map[string]ratelimit.Limits{
		ratelimit.OverrideKindInviteRecipient: {RatePerSecond: 1.0 / 600, Burst: 3},
	}})
	attempt := ratelimit.InviteAttempt{InviterID: 1, TenantID: 1, DomainKey: "project:7", Recipient: "dup@example.com"}

	for i := 0; i < 3; i++ {
		dec, err := th.Check(context.Background(), attempt)
		require.NoError(t, err)
		require.True(t, dec.Allowed, "send %d within the raised burst should pass", i+1)
	}
	fourth, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.False(t, fourth.Allowed, "the fourth rapid send is blocked by the burst-3 override")
}

func TestInviteThrottle_override_tightens_recipient_burst(t *testing.T) {
	// The override can also tighten below the default: burst 1 blocks the second
	// rapid send even though the compiled default would allow two.
	lim := ratelimit.NewCacheLimiter(memory.New())
	base := ratelimit.NewInviteThrottle(lim, ratelimit.DefaultInviteLimits, prometheus.NewRegistry())
	th := base.WithOverrides(stubOverrides{invite: map[string]ratelimit.Limits{
		ratelimit.OverrideKindInviteRecipient: {RatePerSecond: 1.0 / 600, Burst: 1},
	}})
	attempt := ratelimit.InviteAttempt{InviterID: 1, TenantID: 1, DomainKey: "project:7", Recipient: "dup@example.com"}

	first, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, first.Allowed)
	second, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.False(t, second.Allowed, "burst-1 override blocks the second send despite the default of 2")
}

func TestInviteThrottle_inviter_bucket_scoped_by_domain(t *testing.T) {
	// Inviter cap 1/hour. Draining it in one project must not throttle the same
	// inviter in another — the bucket key is scoped by DomainKey so its state
	// matches the per-(tenant,project) granularity of the override that sets it.
	th := newInviteThrottle(t, ratelimit.InviteLimits{InviterPerHour: 1})

	a, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 5, DomainKey: "project:1", Recipient: "a@example.com"})
	require.NoError(t, err)
	require.True(t, a.Allowed)

	drained, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 5, DomainKey: "project:1", Recipient: "b@example.com"})
	require.NoError(t, err)
	require.False(t, drained.Allowed, "same project's inviter bucket is drained")

	other, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 5, DomainKey: "project:2", Recipient: "c@example.com"})
	require.NoError(t, err)
	assert.True(t, other.Allowed, "different project → separate inviter bucket")
}

func TestInviteThrottle_refund_restores_inviter_token(t *testing.T) {
	th := newInviteThrottle(t, ratelimit.InviteLimits{InviterPerHour: 1})
	attempt := ratelimit.InviteAttempt{InviterID: 7, DomainKey: "tenant:1", Recipient: "a@example.com"}

	first, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	require.True(t, first.Allowed)

	blocked, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	require.False(t, blocked.Allowed, "single hourly token spent")

	// The invite create failed → refund returns the token.
	th.Refund(context.Background(), attempt)

	after, err := th.Check(context.Background(), attempt)
	require.NoError(t, err)
	assert.True(t, after.Allowed, "refunded token allows another send")
}

func TestInviteThrottle_nil_refund_is_noop(t *testing.T) {
	var th *ratelimit.InviteThrottle
	assert.NotPanics(t, func() {
		th.Refund(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "tenant:1"})
	})
}

func TestCacheLimiter_refund_caps_at_burst(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	// Refund a cold (already-full, burst 2) bucket: capping keeps it at 2, so
	// only two sends are allowed, not three.
	require.NoError(t, lim.Refund(context.Background(), "k", 1, 2))

	for i := 0; i < 2; i++ {
		dec, err := lim.Allow(context.Background(), "k", 1, 2)
		require.NoError(t, err)
		require.True(t, dec.Allowed)
	}
	dec, err := lim.Allow(context.Background(), "k", 1, 2)
	require.NoError(t, err)
	assert.False(t, dec.Allowed, "refund must not lift the bucket above its burst")
}

func TestInviteThrottle_nil_allows(t *testing.T) {
	var th *ratelimit.InviteThrottle
	dec, err := th.Check(context.Background(), ratelimit.InviteAttempt{InviterID: 1, DomainKey: "tenant:1", Recipient: "a@example.com"})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
}

func uniqueEmail(i int) string {
	return string(rune('a'+i)) + "@example.com"
}
