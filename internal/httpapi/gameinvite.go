package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/webutil"
)

const gameInviteTTL = 5 * time.Minute

var (
	errNotFriends    = errors.New("invite: players are not friends")
	errNotMember     = errors.New("invite: sender is not in the session")
	errSessionClosed = errors.New("invite: session is not open")
	errWrongProject  = errors.New("invite: session belongs to another project")
)

type gameInviteCreateRequest struct {
	ToEmail   string `json:"to_email"`
	SessionID string `json:"session_id"`
}

type gameInviteEntry struct {
	InviteID  int64  `json:"invite_id"`
	FromEmail string `json:"from_email,omitempty"`
	FromXUID  string `json:"from_xuid,omitempty"`
	SessionID string `json:"session_id"`
	JoinCode  string `json:"join_code"`
	ExpiresAt string `json:"expires_at"`
}

// POST /v1/invite
func gameInviteCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req gameInviteCreateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.ToEmail == "" || req.SessionID == "" {
			http.Error(w, "to_email and session_id required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}
		fromUserID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
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
			id, qerr := q.GetEndUserIDByEmail(ctx, sqlcgen.GetEndUserIDByEmailParams{
				ProjectID: projectID,
				Email:     &req.ToEmail,
			})
			if qerr != nil {
				return qerr
			}
			toUserID = id

			// Friends are account-scoped. Resolve both end_users to their
			// linked accounts; an anonymous / unlinked player can't be a
			// friend, so the invite is refused.
			fromAcc, aerr := q.GetEndUserAccountID(ctx, fromUserID)
			if aerr != nil {
				return aerr
			}
			toAcc, aerr := q.GetEndUserAccountID(ctx, toUserID)
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

			sess, qerr := q.GetGameSession(ctx, req.SessionID)
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
			if sess.HostUserID != fromUserID {
				member, merr := q.IsGameSessionMember(ctx, sqlcgen.IsGameSessionMemberParams{
					SessionID: req.SessionID,
					EndUserID: fromUserID,
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
				ProjectID:  projectID,
				FromUserID: fromUserID,
				ToUserID:   toUserID,
				SessionID:  req.SessionID,
				JoinCode:   joinCode,
				ExpiresAt:  pgtype.Timestamptz{Time: now.Add(gameInviteTTL), Valid: true},
			})
			return qerr
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "to_email or session_id not found", http.StatusNotFound)
			return
		case errors.Is(err, errNotFriends):
			http.Error(w, "players are not friends", http.StatusForbidden)
			return
		case errors.Is(err, errNotMember):
			http.Error(w, "sender is not in the session", http.StatusForbidden)
			return
		case errors.Is(err, errSessionClosed), errors.Is(err, errWrongProject):
			http.Error(w, "session is not joinable", http.StatusConflict)
			return
		case err != nil:
			webutil.InternalError(w, "game invite create: tx", err)
			return
		}

		// Best-effort WS push to recipient if connected.
		if d.Hub != nil {
			tenantID, _ := db.TenantFromContext(ctx)
			payload, _ := json.Marshal(map[string]any{
				"invite_id":  inviteID,
				"session_id": req.SessionID,
				"join_code":  joinCode,
			})
			_ = d.Hub.Send(ctx, tenantID, toUserID, realtime.Message{
				Type:    "game_invite",
				Payload: json.RawMessage(payload),
			})
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]int64{"invite_id": inviteID})
	}
}

// DELETE /v1/invite/{id}
// Either the sender (cancel) or recipient (decline/dismiss) may delete an invite.
func gameInviteDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inviteID, ok := pathInt64(r, "id")
		if !ok {
			http.Error(w, "invalid invite id", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		callerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}
		var affected int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			var qerr error
			affected, qerr = sqlcgen.New(tx).DeleteGameInvite(ctx, sqlcgen.DeleteGameInviteParams{
				ID:       inviteID,
				CallerID: callerID,
			})
			return qerr
		})
		if err != nil {
			webutil.InternalError(w, "invite delete: tx", err)
			return
		}
		if affected == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// GET /v1/invite
func gameInviteListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		callerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		var invites []gameInviteEntry
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
			webutil.InternalError(w, "game invite list: tx", err)
			return
		}

		if invites == nil {
			invites = []gameInviteEntry{}
		}
		writeJSON(w, map[string]any{"invites": invites})
	}
}
