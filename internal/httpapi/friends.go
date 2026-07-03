package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/webutil"
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

// POST /v1/friends/{player_id}/request
func friendRequestHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		toUser, ok := pathInt64(r, "player_id")
		if !ok {
			http.Error(w, "player_id required", http.StatusBadRequest)
			return
		}
		fromUser, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		if fromUser == toUser {
			http.Error(w, "cannot friend self", http.StatusBadRequest)
			return
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
			http.Error(w, linkAccountMsg, http.StatusForbidden)
			return
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "target not found", http.StatusNotFound)
			return
		case errors.Is(err, errFriendSelf):
			http.Error(w, "cannot friend self", http.StatusBadRequest)
			return
		case errors.Is(err, errFriendBlocked):
			http.Error(w, "request blocked", http.StatusForbidden)
			return
		case err != nil:
			webutil.InternalError(w, "friend request: tx", err)
			return
		}
		if status == "blocked" {
			http.Error(w, "request blocked", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"status": status})
	}
}

// POST /v1/friends/{player_id}/accept
func friendAcceptHandler(d Deps) http.HandlerFunc {
	return changeStatusHandler(d, "accepted", []string{"pending"})
}

// POST /v1/friends/{player_id}/reject
func friendRejectHandler(d Deps) http.HandlerFunc {
	return changeStatusHandler(d, "rejected", []string{"pending", "accepted"})
}

// DELETE /v1/friends/{player_id}
func friendDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		toUser, ok := pathInt64(r, "player_id")
		if !ok {
			http.Error(w, "player_id required", http.StatusBadRequest)
			return
		}
		fromUser, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
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
			http.Error(w, linkAccountMsg, http.StatusForbidden)
			return
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case err != nil:
			webutil.InternalError(w, "friend delete: tx", err)
			return
		}
		if affected == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// POST /v1/friends/{player_id}/block and /unblock
func friendBlockHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		friendBlockToggle(d, w, r, true)
	}
}

func friendUnblockHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		friendBlockToggle(d, w, r, false)
	}
}

func friendBlockToggle(d Deps, w http.ResponseWriter, r *http.Request, block bool) {
	ctx := r.Context()
	toUser, ok := pathInt64(r, "player_id")
	if !ok {
		http.Error(w, "player_id required", http.StatusBadRequest)
		return
	}
	fromUser, ok := playerauth.IDFromContext(ctx)
	if !ok {
		http.Error(w, "no player", http.StatusUnauthorized)
		return
	}
	if fromUser == toUser {
		http.Error(w, "cannot block self", http.StatusBadRequest)
		return
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
		http.Error(w, linkAccountMsg, http.StatusForbidden)
		return
	case errors.Is(err, errTargetNoAccount):
		http.Error(w, "target not found", http.StatusNotFound)
		return
	case errors.Is(err, errFriendSelf):
		http.Error(w, "cannot block self", http.StatusBadRequest)
		return
	case err != nil:
		webutil.InternalError(w, "friend block: tx", err)
		return
	}
	status := "blocked"
	if !block {
		status = "unblocked"
	}
	writeJSON(w, map[string]any{"status": status})
}

// changeStatusHandler is shared by accept/reject. allowed gates the transition.
func changeStatusHandler(d Deps, newStatus string, allowed []string) http.HandlerFunc {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, s := range allowed {
		allowedSet[s] = struct{}{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// {player_id} is the OTHER user — for accept/reject the "from" of the
		// request is them, "to" is the current user.
		other, ok := pathInt64(r, "player_id")
		if !ok {
			http.Error(w, "player_id required", http.StatusBadRequest)
			return
		}
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
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
			if _, ok := allowedSet[edge.Status]; !ok {
				return errFriendIllegalTransition
			}
			return q.SetFriendEdgeStatusByAccount(ctx, sqlcgen.SetFriendEdgeStatusByAccountParams{
				FromAccountID: otherAcc, ToAccountID: myAcc, Status: newStatus,
			})
		})
		switch {
		case errors.Is(err, errNoAccount):
			http.Error(w, linkAccountMsg, http.StatusForbidden)
			return
		case errors.Is(err, errTargetNoAccount), errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "no pending request", http.StatusNotFound)
			return
		case errors.Is(err, errFriendIllegalTransition):
			http.Error(w, "illegal transition", http.StatusConflict)
			return
		case err != nil:
			webutil.InternalError(w, "friend status: tx", err)
			return
		}
		writeJSON(w, map[string]any{"status": newStatus})
	}
}

// allowedFriendStatuses guards friendsListHandler.
var allowedFriendStatuses = map[string]struct{}{
	"pending":  {},
	"accepted": {},
	"rejected": {},
	"blocked":  {},
}

// GET /v1/friends?status=...
func friendsListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		me, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, _ := db.ProjectFromContext(ctx)
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "accepted"
		}
		if _, allowed := allowedFriendStatuses[status]; !allowed {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		limit := parseLimit(r.URL.Query().Get("limit"), 50, 100)
		cursor := parseCursor(r.URL.Query().Get("cursor"))

		var items []friendEntry
		var lastID int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
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
					if len(playerIDs) > 0 {
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
			http.Error(w, linkAccountMsg, http.StatusForbidden)
			return
		case err != nil:
			webutil.InternalError(w, "friends list: tx", err)
			return
		}
		if items == nil {
			items = []friendEntry{}
		}
		var next string
		if len(items) == int(limit) {
			next = strconv.FormatInt(lastID, 10)
		}
		writeJSON(w, map[string]any{"items": items, "next_cursor": next})
	}
}
