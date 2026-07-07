package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/realtime"
)

const gameInviteTTL = 5 * time.Minute

var (
	errNotFriends    = errors.New("invite: players are not friends")
	errNotMember     = errors.New("invite: sender is not in the session")
	errSessionClosed = errors.New("invite: session is not open")
	errWrongProject  = errors.New("invite: session belongs to another project")
)

// Both fields are required and non-empty at the schema level → a missing or
// blank field is a 422. Recipient resolution (unknown email → 404) and
// session state remain handler/DB concerns.
type gameInviteCreateRequest struct {
	ToEmail   string `json:"to_email" minLength:"1"`
	SessionID string `json:"session_id" minLength:"1"`
}

type gameInviteEntry struct {
	InviteID  int64  `json:"invite_id"`
	FromEmail string `json:"from_email,omitempty"`
	FromXUID  string `json:"from_xuid,omitempty"`
	SessionID string `json:"session_id"`
	JoinCode  string `json:"join_code"`
	ExpiresAt string `json:"expires_at"`
}

type gameInviteCreateInput struct {
	Body gameInviteCreateRequest
}

type gameInviteCreateOutput struct {
	Body struct {
		InviteID int64 `json:"invite_id"`
	}
}

type gameInviteListOutput struct {
	Body struct {
		Invites []gameInviteEntry `json:"invites"`
	}
}

type gameInviteDeleteInput struct {
	ID int64 `path:"id"`
}

// registerGameInvites registers POST/GET/DELETE /v1/invite.
func registerGameInvites(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "createGameInvite",
		Method:        http.MethodPost,
		Path:          "/v1/invite",
		Summary:       "Invite a friend to a game session",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
	}, gameInviteCreate(d))

	huma.Register(api, huma.Operation{
		OperationID: "listGameInvites",
		Method:      http.MethodGet,
		Path:        "/v1/invite",
		Summary:     "List the caller's pending game invites",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, gameInviteList(d))

	huma.Register(api, huma.Operation{
		OperationID:   "deleteGameInvite",
		Method:        http.MethodDelete,
		Path:          "/v1/invite/{id}",
		Summary:       "Cancel or dismiss a game invite",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, gameInviteDelete(d))
}

func gameInviteCreate(d Deps) func(context.Context, *gameInviteCreateInput) (*gameInviteCreateOutput, error) {
	return func(ctx context.Context, in *gameInviteCreateInput) (*gameInviteCreateOutput, error) {
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}
		fromUserID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		now := time.Now()
		var (
			inviteID int64
			toUserID int64
			joinCode string
		)
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)

			// Resolve recipient by email within the project.
			id, qerr := q.GetPlayerIDByEmail(ctx, sqlcgen.GetPlayerIDByEmailParams{
				ProjectID: projectID,
				Email:     &in.Body.ToEmail,
			})
			if qerr != nil {
				return qerr
			}
			toUserID = id

			// Friends are account-scoped. Resolve both project_players to their
			// linked accounts; an anonymous / unlinked player can't be a
			// friend, so the invite is refused.
			fromAcc, aerr := q.GetPlayerLinkedAccountID(ctx, fromUserID)
			if aerr != nil {
				return aerr
			}
			toAcc, aerr := q.GetPlayerLinkedAccountID(ctx, toUserID)
			if aerr != nil {
				return aerr
			}
			if !fromAcc.Valid || !toAcc.Valid {
				return errNotFriends
			}
			// Explicit block gate (either direction) — defense in depth on
			// top of the accepted-friendship requirement below.
			if _, berr := q.IsBlockedBetweenAccounts(ctx, sqlcgen.IsBlockedBetweenAccountsParams{A: fromAcc, B: toAcc}); berr == nil {
				return errNotFriends
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
			// Require an accepted friendship before allowing the invite.
			if _, qerr = q.AreAccountsFriendsAccepted(ctx, sqlcgen.AreAccountsFriendsAcceptedParams{
				A: fromAcc,
				B: toAcc,
			}); qerr != nil {
				if errors.Is(qerr, pgx.ErrNoRows) {
					return errNotFriends
				}
				return qerr
			}

			sess, qerr := q.GetGameSession(ctx, sqlcgen.GetGameSessionParams{ProjectID: projectID, ID: in.Body.SessionID})
			if qerr != nil {
				return qerr
			}
			if sess.ProjectID != projectID {
				return errWrongProject
			}
			if sess.State != "open" || sess.ExpiresAt.Time.Before(now) {
				return errSessionClosed
			}
			// Sender must be the host or an active member of the session —
			// players can't leak join codes for sessions they aren't in.
			if sess.HostPlayerID != fromUserID {
				member, merr := q.IsGameSessionMember(ctx, sqlcgen.IsGameSessionMemberParams{
					SessionID: in.Body.SessionID,
					PlayerID:  fromUserID,
				})
				if merr != nil {
					return merr
				}
				if !member {
					return errNotMember
				}
			}
			joinCode = sess.JoinCode

			inviteID, qerr = q.CreateGameInvite(ctx, sqlcgen.CreateGameInviteParams{
				ProjectID:    projectID,
				FromPlayerID: fromUserID,
				ToPlayerID:   toUserID,
				SessionID:    in.Body.SessionID,
				JoinCode:     joinCode,
				ExpiresAt:    pgtype.Timestamptz{Time: now.Add(gameInviteTTL), Valid: true},
			})
			return qerr
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, huma.Error404NotFound("to_email or session_id not found")
		case errors.Is(err, errNotFriends):
			return nil, huma.Error403Forbidden("players are not friends")
		case errors.Is(err, errNotMember):
			return nil, huma.Error403Forbidden("sender is not in the session")
		case errors.Is(err, errSessionClosed), errors.Is(err, errWrongProject):
			return nil, huma.Error409Conflict("session is not joinable")
		case err != nil:
			return nil, serverError(ctx, "game invite create: tx", err)
		}

		// Best-effort WS push to recipient if connected.
		if d.Hub != nil {
			tenantID, _ := db.TenantFromContext(ctx)
			payload, _ := json.Marshal(map[string]any{
				"invite_id":  inviteID,
				"session_id": in.Body.SessionID,
				"join_code":  joinCode,
			})
			_ = d.Hub.Send(ctx, tenantID, toUserID, realtime.Message{
				Type:    "game_invite",
				Payload: json.RawMessage(payload),
			})
		}

		out := &gameInviteCreateOutput{}
		out.Body.InviteID = inviteID
		return out, nil
	}
}

// gameInviteDelete lets either the sender (cancel) or recipient
// (decline/dismiss) delete an invite.
func gameInviteDelete(d Deps) func(context.Context, *gameInviteDeleteInput) (*struct{}, error) {
	return func(ctx context.Context, in *gameInviteDeleteInput) (*struct{}, error) {
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		var affected int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			var qerr error
			affected, qerr = sqlcgen.New(tx).DeleteGameInvite(ctx, sqlcgen.DeleteGameInviteParams{
				ID:       in.ID,
				CallerID: callerID,
			})
			return qerr
		})
		if err != nil {
			return nil, serverError(ctx, "invite delete: tx", err)
		}
		if affected == 0 {
			return nil, huma.Error404NotFound("not found")
		}
		return nil, nil
	}
}

func gameInviteList(d Deps) func(context.Context, *struct{}) (*gameInviteListOutput, error) {
	return func(ctx context.Context, _ *struct{}) (*gameInviteListOutput, error) {
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		invites := []gameInviteEntry{}
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListPendingGameInvites(ctx, callerID)
			if qerr != nil {
				return qerr
			}
			for _, row := range rows {
				entry := gameInviteEntry{
					InviteID:  row.ID,
					SessionID: row.SessionID,
					JoinCode:  row.JoinCode,
					ExpiresAt: row.ExpiresAt.Time.UTC().Format(time.RFC3339),
				}
				entry.FromEmail = row.FromEmail
				if row.FromXuid != nil {
					entry.FromXUID = *row.FromXuid
				}
				invites = append(invites, entry)
			}
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "game invite list: tx", err)
		}

		out := &gameInviteListOutput{}
		out.Body.Invites = invites
		return out, nil
	}
}
