package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/webutil"
)

// presenceStatusMaxChars matches the DB CHECK (char_length 1..32). Counted in
// runes so multibyte status strings aren't wrongly rejected by a byte count.
const presenceStatusMaxChars = 32

type presenceUpdateRequest struct {
	Status    string  `json:"status"`
	SessionID *string `json:"session_id"`
}

// PUT /v1/presence
func presenceUpdateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req presenceUpdateRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if n := utf8.RuneCountInString(req.Status); n == 0 || n > presenceStatusMaxChars {
			http.Error(w, "status must be 1–32 characters", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		callerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).UpsertPresence(ctx, sqlcgen.UpsertPresenceParams{
				EndUserID: callerID,
				Status:    req.Status,
				SessionID: req.SessionID,
			})
		})
		if err != nil {
			webutil.InternalError(w, "presence update: tx", err)
			return
		}

		// Best-effort WS fan-out to accepted friends' end_users in this
		// project. Friends are account-scoped; a block overwrites the
		// accepted edge, so blocked pairs are excluded here automatically.
		if d.Hub != nil {
			tenantID, _ := db.TenantFromContext(ctx)
			projectID, _ := db.ProjectFromContext(ctx)
			payload, _ := json.Marshal(map[string]any{
				"end_user_id": callerID,
				"status":      req.Status,
				"session_id":  req.SessionID,
			})
			msg := realtime.Message{Type: "presence", Payload: json.RawMessage(payload)}
			for _, friendID := range acceptedFriendEndUsersInProject(ctx, d, callerID, projectID) {
				_ = d.Hub.Send(ctx, tenantID, friendID, msg)
			}
		}

		writeJSON(w, map[string]bool{"ok": true})
	}
}

// acceptedFriendEndUsersInProject resolves the caller's account, lists their
// accepted friend accounts, and maps those accounts back to end_users in the
// given project. Best-effort: any error yields an empty slice (presence
// fan-out is not load-bearing). Anonymous callers have no account → no fan-out.
func acceptedFriendEndUsersInProject(ctx context.Context, d Deps, callerID, projectID int64) []int64 {
	if projectID <= 0 {
		return nil
	}
	var out []int64
	if err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		myAcc, err := q.GetEndUserAccountID(ctx, callerID)
		if err != nil || !myAcc.Valid {
			return err
		}
		rows, err := q.ListFriendsByStatusForAccount(ctx, sqlcgen.ListFriendsByStatusForAccountParams{
			Me: myAcc, Status: "accepted", Cursor: 0, RowLimit: 1000,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		friendAccts := make([]pgtype.UUID, 0, len(rows))
		for _, row := range rows {
			other := row.ToAccountID
			if other == myAcc {
				other = row.FromAccountID
			}
			friendAccts = append(friendAccts, other)
		}
		euRows, err := q.ResolveEndUsersForAccountsInProject(ctx, sqlcgen.ResolveEndUsersForAccountsInProjectParams{
			ProjectID: projectID, AccountIds: friendAccts,
		})
		if err != nil {
			return err
		}
		for _, er := range euRows {
			out = append(out, er.EndUserID)
		}
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "presence fan-out: resolve friends", "err", err)
		return nil
	}
	return out
}
