package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/automoto/gg-scale/internal/auditlog"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
)

type changePasswordInput struct {
	Body struct {
		CurrentPassword string `json:"current_password,omitempty" example:"correct-horse-battery-staple"`
		NewPassword     string `json:"new_password,omitempty" example:"battery-staple-correct-horse"`
	}
}

type disableInput struct {
	Body struct {
		Password string `json:"password,omitempty" example:"correct-horse-battery-staple"`
	}
}

type deleteRequestOutput struct {
	Body struct {
		DeleteRequestedAt time.Time `json:"delete_requested_at"`
		ScheduledPurgeAt  time.Time `json:"scheduled_purge_at"`
	}
}

type deleteCancelInput struct {
	Body struct {
		Email    string `json:"email" format:"email" example:"player@example.com"`
		Password string `json:"password" example:"correct-horse-battery-staple"`
	}
}

// defaultDeleteGracePeriod backs Deps.DeleteGracePeriod when unset, keeping
// test fixtures and sparse deployments on the documented 30-day window.
const defaultDeleteGracePeriod = 720 * time.Hour

func deleteGrace(d Deps) time.Duration {
	if d.DeleteGracePeriod > 0 {
		return d.DeleteGracePeriod
	}
	return defaultDeleteGracePeriod
}

// registerAuthAccountRoutes carries the session-authenticated account
// endpoints (change password, self-disable) — registered in the
// player-session group, unlike the /v1/auth code-sending routes.
func registerAuthAccountRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "changePassword",
		Method:      http.MethodPost,
		Path:        "/v1/auth/password",
		Summary:     "Change the calling player's password",
		Description: "Requires the current password. Revokes every refresh token so " +
			"other devices must sign in again; the caller's live access token " +
			"keeps working until it expires.",
		Tags:          []string{"Authentication"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, changePassword(d))

	huma.Register(api, huma.Operation{
		OperationID: "disablePlayer",
		Method:      http.MethodPost,
		Path:        "/v1/auth/disable",
		Summary:     "Disable the calling player",
		Description: "Self-service deactivation: revokes every session immediately and " +
			"blocks sign-in. Players with credentials must re-authenticate with " +
			"their password; anonymous players disable on the session alone. A " +
			"project admin can re-enable the player with all data intact. " +
			"Permanent deletion is requested with POST /v1/auth/delete.",
		Tags:          []string{"Authentication"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, disablePlayer(d))

	huma.Register(api, huma.Operation{
		OperationID: "requestPlayerDelete",
		Method:      http.MethodPost,
		Path:        "/v1/auth/delete",
		Summary:     "Request deletion of the calling player's data in this project",
		Description: "Disables the player, revokes every session, and schedules a " +
			"permanent deletion of the player's data in this project once the " +
			"grace period passes. Data in other projects is not touched. Players " +
			"with credentials must re-authenticate with their password and can " +
			"cancel with POST /v1/auth/delete/cancel until the purge runs; " +
			"anonymous players cancel through a linked account or the project's " +
			"support.",
		Tags:     []string{"Authentication"},
		Security: playerSecurity,
	}, requestPlayerDelete(d))
}

// playerCredentials reads the caller's stored password hash (nil for
// anonymous / platform-only players). Reads the primary: this feeds
// re-authentication decisions, and a lagging replica could accept a
// just-replaced password or miss a just-linked one.
func playerCredentials(ctx context.Context, d Deps, me int64) (sqlcgen.GetPlayerAuthCredentialsRow, error) {
	var row sqlcgen.GetPlayerAuthCredentialsRow
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		var qerr error
		row, qerr = sqlcgen.New(tx).GetPlayerAuthCredentials(ctx, me)
		return qerr
	})
	return row, err
}

func changePassword(d Deps) func(context.Context, *changePasswordInput) (*struct{}, error) {
	return func(ctx context.Context, in *changePasswordInput) (*struct{}, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		if !validPassword(in.Body.NewPassword) {
			return nil, huma.Error400BadRequest("new_password must be 8–72 characters")
		}

		row, err := playerCredentials(ctx, d, me)
		if err != nil {
			return nil, serverError(ctx, "change password: read", err)
		}
		if len(row.PasswordHash) == 0 {
			return nil, huma.Error409Conflict("player has no sign-in credentials; link an email first (POST /v1/auth/link)")
		}
		if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(in.Body.CurrentPassword)) != nil {
			return nil, huma.Error403Forbidden("current password incorrect")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Body.NewPassword), bcryptCost)
		if err != nil {
			return nil, serverError(ctx, "change password: hash", err)
		}

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if qerr := q.UpdatePlayerPassword(ctx, sqlcgen.UpdatePlayerPasswordParams{
				PasswordHash: hash, ID: me,
			}); qerr != nil {
				return qerr
			}
			if _, qerr := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
				ProjectID: projectID, PlayerID: me,
			}); qerr != nil {
				return qerr
			}
			return auditlog.Write(ctx, tx, me, "auth.password_change", "", nil)
		})
		if err != nil {
			return nil, serverError(ctx, "change password: tx", err)
		}
		return nil, nil
	}
}

func disablePlayer(d Deps) func(context.Context, *disableInput) (*struct{}, error) {
	return func(ctx context.Context, in *disableInput) (*struct{}, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		row, err := playerCredentials(ctx, d, me)
		if err != nil {
			return nil, serverError(ctx, "disable: read", err)
		}
		// Re-auth: a stolen session token alone must not lock the real owner
		// out of a credentialed account.
		if len(row.PasswordHash) > 0 {
			if in.Body.Password == "" {
				return nil, huma.Error400BadRequest("password required to disable this player")
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(in.Body.Password)) != nil {
				return nil, huma.Error403Forbidden("password incorrect")
			}
		}

		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if _, qerr := q.SetPlayerDisabledSelf(ctx, me); qerr != nil {
				return qerr
			}
			if _, qerr := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
				ProjectID: projectID, PlayerID: me,
			}); qerr != nil {
				return qerr
			}
			return auditlog.Write(ctx, tx, me, "auth.disable", "", nil)
		})
		if err != nil {
			return nil, serverError(ctx, "disable: tx", err)
		}
		return nil, nil
	}
}

func requestPlayerDelete(d Deps) func(context.Context, *disableInput) (*deleteRequestOutput, error) {
	return func(ctx context.Context, in *disableInput) (*deleteRequestOutput, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		row, err := playerCredentials(ctx, d, me)
		if err != nil {
			return nil, serverError(ctx, "delete request: read", err)
		}
		// Re-auth: a stolen session token alone must not schedule the real
		// owner's data for permanent deletion.
		if len(row.PasswordHash) > 0 {
			if in.Body.Password == "" {
				return nil, huma.Error400BadRequest("password required to delete this player")
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(in.Body.Password)) != nil {
				return nil, huma.Error403Forbidden("password incorrect")
			}
		}

		out := &deleteRequestOutput{}
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			requestedAt, qerr := q.RequestPlayerDeleteSelf(ctx, me)
			if qerr != nil {
				return qerr
			}
			out.Body.DeleteRequestedAt = requestedAt.Time
			out.Body.ScheduledPurgeAt = requestedAt.Time.Add(deleteGrace(d))
			if _, qerr := q.RevokeActivePlayerSessions(ctx, sqlcgen.RevokeActivePlayerSessionsParams{
				ProjectID: projectID, PlayerID: me,
			}); qerr != nil {
				return qerr
			}
			return auditlog.Write(ctx, tx, me, "auth.delete_request", "",
				map[string]any{"scheduled_purge_at": out.Body.ScheduledPurgeAt})
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error409Conflict("deletion already requested")
		}
		if err != nil {
			return nil, serverError(ctx, "delete request: tx", err)
		}
		return out, nil
	}
}

// authDeleteCancel is registered pre-session (next to login): the delete
// request revoked every session and login filters disabled players, so the
// caller re-authenticates with email + password against the pending row. A
// missing row and a wrong password both answer 404, so the endpoint never
// confirms which emails have a pending deletion.
func authDeleteCancel(d Deps) func(context.Context, *deleteCancelInput) (*struct{}, error) {
	return func(ctx context.Context, in *deleteCancelInput) (*struct{}, error) {
		projectID, _, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}

		errNoPending := huma.Error404NotFound("no pending deletion for these credentials")
		if !validPassword(in.Body.Password) {
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Body.Password))
			return nil, errNoPending
		}
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			email := in.Body.Email
			row, qerr := q.GetPlayerPendingDeleteByEmail(ctx, sqlcgen.GetPlayerPendingDeleteByEmailParams{
				ProjectID: projectID,
				Email:     &email,
			})
			if qerr != nil {
				return qerr
			}
			// A passwordless (account-linked) row must cost the same as a
			// wrong password: an instant ErrHashTooShort return would let
			// response timing confirm the pending deletion.
			if len(row.PasswordHash) == 0 {
				_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Body.Password))
				return errBadCredentials
			}
			if bcrypt.CompareHashAndPassword(row.PasswordHash, []byte(in.Body.Password)) != nil {
				return errBadCredentials
			}
			n, qerr := q.CancelPlayerDeleteSelf(ctx, row.ID)
			if qerr != nil {
				return qerr
			}
			if n == 0 {
				// A concurrent purge won the FOR UPDATE race.
				return pgx.ErrNoRows
			}
			return auditlog.Write(ctx, tx, row.ID, "auth.delete_cancel", "", nil)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(in.Body.Password))
			return nil, errNoPending
		case errors.Is(err, errBadCredentials):
			return nil, errNoPending
		case err != nil:
			return nil, serverError(ctx, "delete cancel: tx", err)
		}
		return nil, nil
	}
}
