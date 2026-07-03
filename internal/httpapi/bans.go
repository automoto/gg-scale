package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// endUserTenantBanned reports whether the end_user's linked account is banned
// in its tenant. Runs in a tenant Pool.Q (end_users RLS-filtered). An
// anonymous / unlinked player can't be tenant-banned, so it returns false.
func endUserTenantBanned(ctx context.Context, d Deps, endUserID int64) (bool, error) {
	var banned bool
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		_, e := sqlcgen.New(tx).IsEndUserBannedByTenant(ctx, endUserID)
		if e == nil {
			banned = true
			return nil
		}
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		return e
	})
	return banned, err
}
