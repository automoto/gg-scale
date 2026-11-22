package controlpanel

import (
	"context"

	"github.com/automoto/gg-scale/internal/projectadmin"
)

// inviteEmailSuppressed reports whether the platform-wide unsubscribe list
// blocks invite mail to email. The check lives in the invite senders — the
// mailer stays a dumb transport. Fails closed on a lookup error.
func (h *Handler) inviteEmailSuppressed(ctx context.Context, email string) bool {
	return projectadmin.EmailSuppressed(ctx, h.pool, email)
}
