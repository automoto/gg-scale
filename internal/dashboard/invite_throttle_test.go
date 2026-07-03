package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/ratelimit"
)

func TestInviteThrottled_denies_second_send_to_same_recipient(t *testing.T) {
	lim := ratelimit.NewCacheLimiter(memory.New())
	th := ratelimit.NewInviteThrottle(lim, ratelimit.InviteLimits{
		InviterPerHour: 100, DomainPerDay: 100, RecipientCooldown: 10 * time.Minute,
	}, prometheus.NewRegistry())
	h := &Handler{inviteThrottle: th}

	retry, throttled := h.inviteThrottled(context.Background(), 1, 7, 0, "dup@example.com")
	assert.False(t, throttled)
	assert.Zero(t, retry)

	retry, throttled = h.inviteThrottled(context.Background(), 1, 7, 0, "dup@example.com")
	assert.True(t, throttled, "second send to same recipient within cooldown is throttled")
	assert.GreaterOrEqual(t, retry, 1, "Retry-After floored at 1s")
}

func TestInviteThrottled_nil_throttle_allows(t *testing.T) {
	h := &Handler{}
	retry, throttled := h.inviteThrottled(context.Background(), 1, 7, 0, "a@example.com")
	assert.False(t, throttled)
	assert.Zero(t, retry)
}
