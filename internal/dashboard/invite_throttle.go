package dashboard

import (
	"context"
	"log/slog"
	"math"
	"strconv"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

// inviteThrottled checks the per-inviter / per-domain / per-recipient invite
// buckets. It returns the Retry-After (seconds, floored at 1) and true when the
// send should be refused. A cache-backend error fails open (allows the send) —
// blocking a legitimate admin's invite on transient cache trouble is worse than
// briefly relaxing an abuse cap.
func (h *Handler) inviteThrottled(ctx context.Context, inviterID, tenantID, projectID int64, recipient string) (int, bool) {
	if h.inviteThrottle == nil {
		return 0, false
	}
	domainKey := tenantDomainKey(tenantID)
	if projectID > 0 {
		domainKey = projectDomainKey(projectID)
	}
	dec, err := h.inviteThrottle.Check(ctx, ratelimit.InviteAttempt{
		InviterID: inviterID,
		TenantID:  tenantID,
		ProjectID: projectID,
		DomainKey: domainKey,
		Recipient: recipient,
	})
	if err != nil {
		slog.ErrorContext(ctx, "invite throttle check", "err", err)
		return 0, false
	}
	if dec.Allowed {
		return 0, false
	}
	retry := int(math.Ceil(dec.RetryAfter.Seconds()))
	if retry < 1 {
		retry = 1
	}
	return retry, true
}

// inviteRefund credits back the tokens a prior inviteThrottled debited. Call it
// when the invite create fails after the throttle allowed the send, so a
// duplicate/transient failure doesn't permanently burn the inviter's or
// domain's quota. No-op when the throttle is unwired.
func (h *Handler) inviteRefund(ctx context.Context, inviterID, tenantID, projectID int64, recipient string) {
	if h.inviteThrottle == nil {
		return
	}
	domainKey := tenantDomainKey(tenantID)
	if projectID > 0 {
		domainKey = projectDomainKey(projectID)
	}
	h.inviteThrottle.Refund(ctx, ratelimit.InviteAttempt{
		InviterID: inviterID,
		TenantID:  tenantID,
		ProjectID: projectID,
		DomainKey: domainKey,
		Recipient: recipient,
	})
}

func projectDomainKey(projectID int64) string {
	return "project:" + strconv.FormatInt(projectID, 10)
}

func tenantDomainKey(tenantID int64) string {
	return "tenant:" + strconv.FormatInt(tenantID, 10)
}
