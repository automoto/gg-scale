package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

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

		// Best-effort WS fan-out to all accepted friends.
		if d.Hub != nil {
			tenantID, _ := db.TenantFromContext(ctx)
			payload, _ := json.Marshal(map[string]any{
				"end_user_id": callerID,
				"status":      req.Status,
				"session_id":  req.SessionID,
			})
			msg := realtime.Message{Type: "presence", Payload: json.RawMessage(payload)}
			var friendRows []sqlcgen.ListAcceptedFriendIDsRow
			if qerr := d.Pool.Q(ctx, func(tx pgx.Tx) error {
				var e error
				friendRows, e = sqlcgen.New(tx).ListAcceptedFriendIDs(ctx, callerID)
				return e
			}); qerr != nil {
				slog.WarnContext(ctx, "presence fan-out: list friends", "err", qerr)
			}
			for _, row := range friendRows {
				friendID := row.ToUserID
				if friendID == callerID {
					friendID = row.FromUserID
				}
				_ = d.Hub.Send(ctx, tenantID, friendID, msg)
			}
		}

		writeJSON(w, map[string]bool{"ok": true})
	}
}
