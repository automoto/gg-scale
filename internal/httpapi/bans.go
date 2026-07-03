package httpapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
)

// epochValidator adapts the app pool to playerauth.EpochValidator so the
// player middleware can reject a token whose session_epoch is stale (the player
// was banned, disabled, or changed their password after it was minted). Runs in
// a tenant Pool.Q, so project_players RLS scopes the lookup to the caller's
// tenant; a missing row (deleted / cross-tenant player) maps to ErrRevoked.
type epochValidator struct{ pool *db.Pool }

func (e epochValidator) CurrentEpoch(ctx context.Context, playerID int64) (int64, error) {
	var epoch int32
	err := e.pool.Q(ctx, func(tx pgx.Tx) error {
		v, qerr := sqlcgen.New(tx).GetPlayerSessionEpoch(ctx, playerID)
		epoch = v
		return qerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, playerauth.ErrRevoked
	}
	if err != nil {
		return 0, err
	}
	return int64(epoch), nil
}

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
