package controlpanel

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// inviteEmailSuppressed reports whether the platform-wide unsubscribe list
// blocks invite mail to email. The check lives in the invite senders — the
// mailer stays a dumb transport. Fails closed on a lookup error: better to
// drop one invite email than mail an unsubscribed address.
func (h *Handler) inviteEmailSuppressed(ctx context.Context, email string) bool {
	var suppressed bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var qerr error
		suppressed, qerr = sqlcgen.New(tx).IsEmailSuppressed(ctx, strings.ToLower(strings.TrimSpace(email)))
		return qerr
	})
	if err != nil {
		slog.ErrorContext(ctx, "invite suppression lookup", "err", err)
		return true
	}
	if suppressed {
		slog.InfoContext(ctx, "invite email dropped: address unsubscribed", "email", email)
	}
	return suppressed
}
