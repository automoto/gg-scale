package httpapi

import (
	"context"
	"net/http"

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
			"Permanent deletion is requested through the account pages.",
		Tags:          []string{"Authentication"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, disablePlayer(d))
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
