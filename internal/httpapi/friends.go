package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
)

type friendEntry struct {
	ID         int64  `json:"id"`
	FromUserID int64  `json:"from_user_id"`
	ToUserID   int64  `json:"to_user_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// POST /v1/friends/{user_id}/request — m1.md 4.4.1.
func friendRequestHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		toUser, ok := pathInt64(r, "user_id")
		if !ok {
			http.Error(w, "user_id required", http.StatusBadRequest)
			return
		}
		fromUser, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}
		if fromUser == toUser {
			http.Error(w, "cannot friend self", http.StatusBadRequest)
			return
		}

		var status string
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			row, err := q.RequestFriend(ctx, sqlcgen.RequestFriendParams{
				FromUserID: fromUser, ToUserID: toUser,
			})
			if err == nil {
				status = row.Status
				return nil
			}
			// ON CONFLICT DO UPDATE WHERE clause failed → row exists but
			// is already pending/accepted/blocked. Surface its current
			// status (idempotent for pending/accepted; 403 for blocked).
			if errors.Is(err, pgx.ErrNoRows) {
				existing, gerr := q.GetFriendEdge(ctx, sqlcgen.GetFriendEdgeParams{
					FromUserID: fromUser, ToUserID: toUser,
				})
				if gerr != nil {
					return gerr
				}
				status = existing.Status
				return nil
			}
			return err
		})
		if err != nil {
			internalError(w, "friend request: tx", err)
			return
		}

		if status == "blocked" {
			http.Error(w, "request blocked", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"status": status})
	}
}

// POST /v1/friends/{user_id}/accept — m1.md 4.4.2.
func friendAcceptHandler(d Deps) http.HandlerFunc {
	return changeStatusHandler(d, "accepted", []string{"pending"})
}

// POST /v1/friends/{user_id}/reject — m1.md 4.4.2.
func friendRejectHandler(d Deps) http.HandlerFunc {
	return changeStatusHandler(d, "rejected", []string{"pending", "accepted"})
}

// DELETE /v1/friends/{user_id} — m1.md 4.4.2.
func friendDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		toUser, ok := pathInt64(r, "user_id")
		if !ok {
			http.Error(w, "user_id required", http.StatusBadRequest)
			return
		}
		fromUser, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).DeleteFriendEdge(ctx, sqlcgen.DeleteFriendEdgeParams{
				FromUserID: fromUser, ToUserID: toUser,
			})
		})
		if err != nil {
			internalError(w, "friend delete: tx", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// changeStatusHandler is shared by accept/reject. allowedFromStatuses gates
// the transition; if the existing status isn't in the list we 409.
func changeStatusHandler(d Deps, newStatus string, allowed []string) http.HandlerFunc {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, s := range allowed {
		allowedSet[s] = struct{}{}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// {user_id} is the OTHER user — for accept/reject the "from" of the
		// request is them, "to" is the current user.
		other, ok := pathInt64(r, "user_id")
		if !ok {
			http.Error(w, "user_id required", http.StatusBadRequest)
			return
		}
		me, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			edge, err := q.GetFriendEdge(ctx, sqlcgen.GetFriendEdgeParams{
				FromUserID: other, ToUserID: me,
			})
			if err != nil {
				return err
			}
			if _, ok := allowedSet[edge.Status]; !ok {
				return errFriendIllegalTransition
			}
			return q.SetFriendEdgeStatus(ctx, sqlcgen.SetFriendEdgeStatusParams{
				FromUserID: other, ToUserID: me, Status: newStatus,
			})
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "no pending request", http.StatusNotFound)
			return
		case errors.Is(err, errFriendIllegalTransition):
			http.Error(w, "illegal transition", http.StatusConflict)
			return
		case err != nil:
			internalError(w, "friend status: tx", err)
			return
		}
		writeJSON(w, map[string]any{"status": newStatus})
	}
}

// GET /v1/friends?status=... — m1.md 4.4.3.
func friendsListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		me, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "accepted"
		}
		limit := parseLimit(r.URL.Query().Get("limit"), 50, 100)
		cursor := parseCursor(r.URL.Query().Get("cursor"))

		var items []friendEntry
		var lastID int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListFriendsByStatus(ctx, sqlcgen.ListFriendsByStatusParams{
				FromUserID: me, Status: status, ID: cursor, Limit: limit,
			})
			if qerr != nil {
				return qerr
			}
			for _, row := range rows {
				items = append(items, friendEntry{
					ID: row.ID, FromUserID: row.FromUserID, ToUserID: row.ToUserID,
					Status:    row.Status,
					CreatedAt: row.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
					UpdatedAt: row.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
				})
				lastID = row.ID
			}
			return nil
		})
		if err != nil {
			internalError(w, "friends list: tx", err)
			return
		}
		var next string
		if len(items) == int(limit) {
			next = strconv.FormatInt(lastID, 10)
		}
		writeJSON(w, map[string]any{"items": items, "next_cursor": next})
	}
}

var errFriendIllegalTransition = errors.New("friend: illegal status transition")
