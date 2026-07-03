package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// playerTenantBanned reports whether the player's linked account is banned
// in its tenant. Runs in a tenant Pool.Q (project_players RLS-filtered). An
// anonymous / unlinked player can't be tenant-banned, so it returns false.
func playerTenantBanned(ctx context.Context, d Deps, playerID int64) (bool, error) {
	var banned bool
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		_, e := sqlcgen.New(tx).IsPlayerBannedByTenant(ctx, playerID)
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
