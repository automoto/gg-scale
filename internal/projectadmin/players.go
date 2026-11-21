package projectadmin

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
)

// Errors of the player writes. Management API clients see their text, so it
// has no package prefix.
var (
	// ErrPlayerNotFound means the player is not in the project, or is gone.
	ErrPlayerNotFound = errors.New("player not found")
	// ErrPlayerDeletePending means a pending deletion owns the disabled state.
	ErrPlayerDeletePending = errors.New("deletion pending; cancel the deletion first")
	// ErrPlayerDeleteRequested means an administrator already requested the deletion.
	ErrPlayerDeleteRequested = errors.New("deletion already requested")
	// ErrNoPendingDelete means the player has no pending deletion to cancel.
	ErrNoPendingDelete = errors.New("no pending deletion")
)

// defaultDeleteGracePeriod is the grace period when none is configured.
const defaultDeleteGracePeriod = 720 * time.Hour

// DeleteGrace returns configured, or the default when it is not set.
func DeleteGrace(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return defaultDeleteGracePeriod
}

// playerGone returns the error for a guarded write that matched no row: the
// player is either pending deletion (pending) or not in the project.
func playerGone(ctx context.Context, q *sqlcgen.Queries, tenantID, projectID, playerID int64, pending error) error {
	row, err := q.GetPlayerForProject(ctx, sqlcgen.GetPlayerForProjectParams{TenantID: tenantID, ProjectID: projectID, ID: playerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPlayerNotFound
	}
	if err != nil {
		return err
	}
	if row.DeleteRequestedAt.Valid {
		return pending
	}
	return ErrPlayerNotFound
}

// SetPlayerDisabled disables or enables a player in one project and bumps its
// session epoch, so live tokens stop working. at is the disable time.
func SetPlayerDisabled(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, disabled bool, at time.Time, actor Actor) error {
	var disabledAt pgtype.Timestamptz
	action := "control_panel.player.enable"
	if disabled {
		disabledAt = pgtype.Timestamptz{Time: at, Valid: true}
		action = "control_panel.player.disable"
	}
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		updated, err := q.SetPlayerDisabledInProject(ctx, sqlcgen.SetPlayerDisabledInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID, DisabledAt: disabledAt,
		})
		if err != nil {
			return err
		}
		if updated == 0 {
			// The guarded UPDATE skips players with a pending deletion.
			return playerGone(ctx, q, tenantID, projectID, playerID, ErrPlayerDeletePending)
		}
		return actor.audit(ctx, tx, tenantID, projectID, action, strconv.FormatInt(playerID, 10), nil)
	})
}

// RequestPlayerDelete disables the player, ends its sessions, and starts the
// grace period. It returns the time the purge sweep may delete the data.
func RequestPlayerDelete(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, grace time.Duration, actor Actor) (time.Time, error) {
	var purgeAt time.Time
	ctx = db.WithTenant(ctx, tenantID)
	err := pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		requestedAt, err := q.RequestPlayerDeleteInProject(ctx, sqlcgen.RequestPlayerDeleteInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return playerGone(ctx, q, tenantID, projectID, playerID, ErrPlayerDeleteRequested)
		}
		if err != nil {
			return err
		}
		purgeAt = requestedAt.Time.Add(grace)
		if _, err := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
			ProjectID: projectID, PlayerID: playerID,
		}); err != nil {
			return err
		}
		return actor.audit(ctx, tx, tenantID, projectID, "control_panel.player.delete_request",
			strconv.FormatInt(playerID, 10), map[string]any{"scheduled_purge_at": purgeAt})
	})
	return purgeAt, err
}

// CancelPlayerDelete ends a pending deletion. It returns ErrNoPendingDelete
// when the player has none.
func CancelPlayerDelete(ctx context.Context, pool *db.Pool, tenantID, projectID, playerID int64, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		cancelled, err := sqlcgen.New(tx).CancelPlayerDeleteInProject(ctx, sqlcgen.CancelPlayerDeleteInProjectParams{
			ID: playerID, ProjectID: projectID, TenantID: tenantID,
		})
		if err != nil {
			return err
		}
		if cancelled == 0 {
			return ErrNoPendingDelete
		}
		return actor.audit(ctx, tx, tenantID, projectID, "control_panel.player.delete_cancel", strconv.FormatInt(playerID, 10), nil)
	})
}
