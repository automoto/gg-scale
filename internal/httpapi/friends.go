package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/playerauth"
)

// Friends are between GLOBAL player_accounts. The JSON API caller is an
// player with a session; the operation resolves the caller's linked account
// (403 if the player is anonymous / unlinked) and the target player's
// account, then works on account-to-account edges. See docs/temp/player-accounts.md.

var (
	errFriendIllegalTransition = errors.New("friend: illegal status transition")
	errNoAccount               = errors.New("friend: caller has no linked account")
	errTargetNoAccount         = errors.New("friend: target has no linked account")
	errFriendBlocked           = errors.New("friend: interaction blocked")
	errFriendSelf              = errors.New("friend: cannot friend self")
)

const linkAccountMsg = "link a gg-scale account to use friends"

type friendPresence struct {
	Status    string  `json:"status"`
	SessionID *string `json:"session_id"`
}

type friendEntry struct {
	ID          int64           `json:"id"`
	AccountID   string          `json:"account_id"`
	PlayerID    *int64          `json:"player_id,omitempty"`
	Status      string          `json:"status"`
	Email       *string         `json:"email,omitempty"`
	DisplayName *string         `json:"display_name,omitempty"`
	Presence    *friendPresence `json:"presence,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

// callerAccount resolves the linked account UUID for the current player.
// Returns errNoAccount when the player is anonymous / unlinked.
func callerAccount(ctx context.Context, tx pgx.Tx, playerID int64) (pgtype.UUID, error) {
	acc, err := sqlcgen.New(tx).GetPlayerLinkedAccountID(ctx, playerID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if !acc.Valid {
		return pgtype.UUID{}, errNoAccount
	}
	return acc, nil
}

type friendTargetInput struct {
	PlayerID int64 `path:"player_id" minimum:"1"`
}

type friendStatusResult struct {
	Status string `json:"status"`
}

type friendStatusOutput struct {
	Body friendStatusResult
}

type friendsListInput struct {
	Status string `query:"status"`
	Limit  string `query:"limit"`
	Cursor string `query:"cursor"`
}

type friendsListResult struct {
	Items      []friendEntry `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

type friendsListOutput struct {
	Body friendsListResult
}

func registerFriendRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "listFriends",
		Method:      http.MethodGet,
		Path:        "/v1/friends",
		Summary:     "List the caller's friends by status",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendsList(d))

	huma.Register(api, huma.Operation{
		OperationID: "requestFriend",
		Method:      http.MethodPost,
		Path:        "/v1/friends/{player_id}/request",
		Summary:     "Send a friend request",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendRequest(d))

	huma.Register(api, huma.Operation{
		OperationID: "acceptFriend",
		Method:      http.MethodPost,
		Path:        "/v1/friends/{player_id}/accept",
		Summary:     "Accept a friend request",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendAccept(d))

	huma.Register(api, huma.Operation{
		OperationID: "rejectFriend",
		Method:      http.MethodPost,
		Path:        "/v1/friends/{player_id}/reject",
		Summary:     "Reject a friend request",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendReject(d))

	huma.Register(api, huma.Operation{
		OperationID: "blockFriend",
		Method:      http.MethodPost,
		Path:        "/v1/friends/{player_id}/block",
		Summary:     "Block a player",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendBlock(d))

	huma.Register(api, huma.Operation{
		OperationID: "unblockFriend",
		Method:      http.MethodPost,
		Path:        "/v1/friends/{player_id}/unblock",
		Summary:     "Unblock a player",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, friendUnblock(d))

	huma.Register(api, huma.Operation{
		OperationID:   "deleteFriend",
		Method:        http.MethodDelete,
		Path:          "/v1/friends/{player_id}",
		Summary:       "Remove a friend",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, friendDelete(d))
}

func friendRequest(d Deps) func(context.Context, *friendTargetInput) (*friendStatusOutput, error) {
	return func(ctx context.Context, in *friendTargetInput) (*friendStatusOutput, error) {
		toUser := in.PlayerID
		fromUser, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		if fromUser == toUser {
			return nil, huma.Error400BadRequest("cannot friend self")
		}

		var status string
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			fromAcc, err := callerAccount(ctx, tx, fromUser)
			if err != nil {
				return err
			}
			toAcc, err := q.GetPlayerLinkedAccountID(ctx, toUser)
			if err != nil {
				return err
			}
			if !toAcc.Valid {
				return errTargetNoAccount
			}
			if fromAcc == toAcc {
				return errFriendSelf
			}
			// Block gate (either direction).
			if _, berr := q.IsBlockedBetweenAccounts(ctx, sqlcgen.IsBlockedBetweenAccountsParams{A: fromAcc, B: toAcc}); berr == nil {
				return errFriendBlocked
			} else if !errors.Is(berr, pgx.ErrNoRows) {
				return berr
			}
			row, err := q.RequestFriendByAccount(ctx, sqlcgen.RequestFriendByAccountParams{
				FromAccountID: fromAcc, ToAccountID: toAcc,
			})
			if err == nil {
				status = row.Status
				return nil
			}
			if errors.Is(err, pgx.ErrNoRows) {
				existing, gerr := q.GetFriendEdgeByAccount(ctx, sqlcgen.GetFriendEdgeByAccountParams{
					FromAccountID: fromAcc, ToAccountID: toAcc,
				})
				if gerr != nil {
					return gerr
				}
				status = existing.Status
				return nil
			}
			return err
		})
		switch {
		case errors.Is(err, errNoAccount):
			return nil, huma.Error403Forbidden(linkAccountMsg)
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows), errors.Is(err, errFriendBlocked):
			// A block never reveals itself to the blockee: a blocked request is
			// indistinguishable from one to a non-existent target.
			return nil, huma.Error404NotFound("target not found")
		case errors.Is(err, errFriendSelf):
			return nil, huma.Error400BadRequest("cannot friend self")
		case err != nil:
			return nil, serverError(ctx, "friend request: tx", err)
		}
		if status == "blocked" {
			return nil, huma.Error404NotFound("target not found")
		}
		d.Metrics.FriendRequest(observability.FriendRequestSent)
		return &friendStatusOutput{Body: friendStatusResult{Status: status}}, nil
	}
}

func friendAccept(d Deps) func(context.Context, *friendTargetInput) (*friendStatusOutput, error) {
	return func(ctx context.Context, in *friendTargetInput) (*friendStatusOutput, error) {
		return changeFriendStatus(ctx, d, in.PlayerID, "accepted", []string{"pending"})
	}
}

func friendReject(d Deps) func(context.Context, *friendTargetInput) (*friendStatusOutput, error) {
	return func(ctx context.Context, in *friendTargetInput) (*friendStatusOutput, error) {
		return changeFriendStatus(ctx, d, in.PlayerID, "rejected", []string{"pending", "accepted"})
	}
}

func friendDelete(d Deps) func(context.Context, *friendTargetInput) (*struct{}, error) {
	return func(ctx context.Context, in *friendTargetInput) (*struct{}, error) {
		toUser := in.PlayerID
		fromUser, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		var affected int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			fromAcc, err := callerAccount(ctx, tx, fromUser)
			if err != nil {
				return err
			}
			toAcc, err := q.GetPlayerLinkedAccountID(ctx, toUser)
			if err != nil {
				return err
			}
			if !toAcc.Valid {
				return errTargetNoAccount
			}
			n, qerr := q.DeleteFriendEdgeByAccount(ctx, sqlcgen.DeleteFriendEdgeByAccountParams{
				Me: fromAcc, Other: toAcc,
			})
			affected = n
			return qerr
		})
		switch {
		case errors.Is(err, errNoAccount):
			return nil, huma.Error403Forbidden(linkAccountMsg)
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
			return nil, huma.Error404NotFound("not found")
		case err != nil:
			return nil, serverError(ctx, "friend delete: tx", err)
		}
		if affected == 0 {
			return nil, huma.Error404NotFound("not found")
		}
		return nil, nil
	}
}

func friendBlock(d Deps) func(context.Context, *friendTargetInput) (*friendStatusOutput, error) {
	return func(ctx context.Context, in *friendTargetInput) (*friendStatusOutput, error) {
		return friendBlockToggle(ctx, d, in.PlayerID, true)
	}
}

func friendUnblock(d Deps) func(context.Context, *friendTargetInput) (*friendStatusOutput, error) {
	return func(ctx context.Context, in *friendTargetInput) (*friendStatusOutput, error) {
		return friendBlockToggle(ctx, d, in.PlayerID, false)
	}
}

func friendBlockToggle(ctx context.Context, d Deps, toUser int64, block bool) (*friendStatusOutput, error) {
	fromUser, ok := playerauth.IDFromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("no player")
	}
	if fromUser == toUser {
		return nil, huma.Error400BadRequest("cannot block self")
	}

	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		fromAcc, err := callerAccount(ctx, tx, fromUser)
		if err != nil {
			return err
		}
		toAcc, err := q.GetPlayerLinkedAccountID(ctx, toUser)
		if err != nil {
			return err
		}
		if !toAcc.Valid {
			return errTargetNoAccount
		}
		if fromAcc == toAcc {
			return errFriendSelf
		}
		if block {
			// Overwrite any prior edge; also clear the reverse edge so a
			// blocked pair shares no accepted/pending state.
			if _, derr := q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
				FromAccountID: toAcc, ToAccountID: fromAcc, Status: "accepted",
			}); derr != nil {
				return derr
			}
			_, _ = q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
				FromAccountID: toAcc, ToAccountID: fromAcc, Status: "pending",
			})
			return q.UpsertFriendEdgeStatusByAccount(ctx, sqlcgen.UpsertFriendEdgeStatusByAccountParams{
				FromAccountID: fromAcc, ToAccountID: toAcc, Status: "blocked",
			})
		}
		_, derr := q.DeleteFriendEdgeDirected(ctx, sqlcgen.DeleteFriendEdgeDirectedParams{
			FromAccountID: fromAcc, ToAccountID: toAcc, Status: "blocked",
		})
		return derr
	})
	switch {
	case errors.Is(err, errNoAccount):
		return nil, huma.Error403Forbidden(linkAccountMsg)
	// A nonexistent target id (ErrNoRows) must be indistinguishable from an
	// unlinked one.
	case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
		return nil, huma.Error404NotFound("target not found")
	case errors.Is(err, errFriendSelf):
		return nil, huma.Error400BadRequest("cannot block self")
	case err != nil:
		return nil, serverError(ctx, "friend block: tx", err)
	}
	status := "blocked"
	if !block {
		status = "unblocked"
	}
	return &friendStatusOutput{Body: friendStatusResult{Status: status}}, nil
}

// changeFriendStatus is shared by accept/reject. allowed gates the transition.
func changeFriendStatus(ctx context.Context, d Deps, other int64, newStatus string, allowed []string) (*friendStatusOutput, error) {
	// {player_id} is the OTHER user — for accept/reject the "from" of the
	// request is them, "to" is the current user.
	me, ok := playerauth.IDFromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("no player")
	}

	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		myAcc, err := callerAccount(ctx, tx, me)
		if err != nil {
			return err
		}
		otherAcc, err := q.GetPlayerLinkedAccountID(ctx, other)
		if err != nil {
			return err
		}
		if !otherAcc.Valid {
			return errTargetNoAccount
		}
		edge, err := q.GetFriendEdgeByAccount(ctx, sqlcgen.GetFriendEdgeByAccountParams{
			FromAccountID: otherAcc, ToAccountID: myAcc,
		})
		if err != nil {
			return err
		}
		if !slices.Contains(allowed, edge.Status) {
			return errFriendIllegalTransition
		}
		return q.SetFriendEdgeStatusByAccount(ctx, sqlcgen.SetFriendEdgeStatusByAccountParams{
			FromAccountID: otherAcc, ToAccountID: myAcc, Status: newStatus,
		})
	})
	switch {
	case errors.Is(err, errNoAccount):
		return nil, huma.Error403Forbidden(linkAccountMsg)
	case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
		return nil, huma.Error404NotFound("no pending request")
	case errors.Is(err, errFriendIllegalTransition):
		return nil, huma.Error409Conflict("illegal transition")
	case err != nil:
		return nil, serverError(ctx, "friend status: tx", err)
	}
	switch newStatus {
	case "accepted":
		d.Metrics.FriendRequest(observability.FriendRequestAccepted)
	case "rejected":
		d.Metrics.FriendRequest(observability.FriendRequestDeclined)
	}
	return &friendStatusOutput{Body: friendStatusResult{Status: newStatus}}, nil
}

// allowedFriendStatuses guards the friends list.
var allowedFriendStatuses = map[string]struct{}{
	"pending":  {},
	"accepted": {},
	"rejected": {},
	"blocked":  {},
}

func friendsList(d Deps) func(context.Context, *friendsListInput) (*friendsListOutput, error) {
	return func(ctx context.Context, in *friendsListInput) (*friendsListOutput, error) {
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, _ := db.ProjectFromContext(ctx)
		status := in.Status
		if status == "" {
			status = "accepted"
		}
		if _, allowed := allowedFriendStatuses[status]; !allowed {
			return nil, huma.Error400BadRequest("invalid status")
		}
		limit := parseLimit(in.Limit, 50, 100)
		cursor := parseCursor(in.Cursor)

		var items []friendEntry
		var lastID int64
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			myAcc, err := callerAccount(ctx, tx, me)
			if err != nil {
				return err
			}
			rows, qerr := q.ListFriendsByStatusForAccount(ctx, sqlcgen.ListFriendsByStatusForAccountParams{
				Me: myAcc, Status: status, Cursor: cursor, RowLimit: limit,
			})
			if qerr != nil {
				return qerr
			}

			friendAccts := make([]pgtype.UUID, 0, len(rows))
			for _, row := range rows {
				other := row.ToAccountID
				if other == myAcc {
					other = row.FromAccountID
				}
				friendAccts = append(friendAccts, other)
			}

			idMap := map[pgtype.UUID]sqlcgen.ListAccountIdentitiesRow{}
			playerByAccount := map[pgtype.UUID]int64{}
			presMap := map[int64]sqlcgen.ListPresenceForUsersRow{}
			if len(friendAccts) > 0 {
				idRows, qerr := q.ListAccountIdentities(ctx, friendAccts)
				if qerr != nil {
					return qerr
				}
				for _, ir := range idRows {
					idMap[ir.ID] = ir
				}
				// Resolve each friend account to a player in the caller's
				// project for player_id + presence (best-effort).
				if projectID > 0 {
					playerRows, qerr := q.ResolvePlayersForAccountsInProject(ctx, sqlcgen.ResolvePlayersForAccountsInProjectParams{
						ProjectID: projectID, AccountIds: friendAccts,
					})
					if qerr != nil {
						return qerr
					}
					playerIDs := make([]int64, 0, len(playerRows))
					for _, pr := range playerRows {
						playerByAccount[pr.PlayerAccountID] = pr.PlayerID
						playerIDs = append(playerIDs, pr.PlayerID)
					}
					// Presence (online status + current session) is shared only
					// between accepted friends. Never enrich a pending/blocked
					// list — otherwise sending a friend request would leak the
					// target's live presence without their consent.
					if status == "accepted" && len(playerIDs) > 0 {
						presRows, qerr := q.ListPresenceForUsers(ctx, playerIDs)
						if qerr != nil {
							return qerr
						}
						for _, pr := range presRows {
							presMap[pr.PlayerID] = pr
						}
					}
				}
			}

			for _, row := range rows {
				other := row.ToAccountID
				if other == myAcc {
					other = row.FromAccountID
				}
				entry := friendEntry{
					ID:        row.ID,
					AccountID: uuid.UUID(other.Bytes).String(),
					Status:    row.Status,
					CreatedAt: row.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
					UpdatedAt: row.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
				}
				if ir, ok := idMap[other]; ok {
					if ir.Email != "" {
						email := ir.Email
						entry.Email = &email
					}
					entry.DisplayName = ir.DisplayName
				}
				if pid, ok := playerByAccount[other]; ok {
					id := pid
					entry.PlayerID = &id
					if pr, ok := presMap[pid]; ok {
						entry.Presence = &friendPresence{Status: pr.Status, SessionID: pr.SessionID}
					}
				}
				items = append(items, entry)
				lastID = row.ID
			}
			return nil
		})
		switch {
		case errors.Is(err, errNoAccount):
			return nil, huma.Error403Forbidden(linkAccountMsg)
		case err != nil:
			return nil, serverError(ctx, "friends list: tx", err)
		}
		if items == nil {
			items = []friendEntry{}
		}
		var next string
		if len(items) == int(limit) {
			next = strconv.FormatInt(lastID, 10)
		}
		return &friendsListOutput{Body: friendsListResult{Items: items, NextCursor: next}}, nil
	}
}
