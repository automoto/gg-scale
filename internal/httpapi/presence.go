package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/realtime"
)

// Status length is enforced by the schema (huma counts runes, matching the DB
// CHECK char_length 1..32) → a missing/empty/oversize status is a 422.
// session_id is optional.
type presenceUpdateRequest struct {
	Status    string  `json:"status" minLength:"1" maxLength:"32"`
	SessionID *string `json:"session_id,omitempty"`
}

type presenceUpdateInput struct {
	Body presenceUpdateRequest
}

type presenceUpdateOutput struct {
	Body okResult
}

// okResult is the {"ok": true} acknowledgement body shared by fire-and-forget
// player endpoints.
type okResult struct {
	OK bool `json:"ok"`
}

// registerPresence registers PUT /v1/presence.
func registerPresence(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "updatePresence",
		Method:      http.MethodPut,
		Path:        "/v1/presence",
		Summary:     "Update the caller's presence",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, func(ctx context.Context, in *presenceUpdateInput) (*presenceUpdateOutput, error) {
		callerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).UpsertPresence(ctx, sqlcgen.UpsertPresenceParams{
				PlayerID:  callerID,
				Status:    in.Body.Status,
				SessionID: in.Body.SessionID,
			})
		})
		if err != nil {
			return nil, serverError(ctx, "presence update: tx", err)
		}

		// Best-effort WS fan-out to accepted friends' project_players in this
		// project. Friends are account-scoped; a block overwrites the
		// accepted edge, so blocked pairs are excluded here automatically.
		if d.Hub != nil {
			tenantID, _ := db.TenantFromContext(ctx)
			projectID, _ := db.ProjectFromContext(ctx)
			payload, _ := json.Marshal(map[string]any{
				"player_id":  callerID,
				"status":     in.Body.Status,
				"session_id": in.Body.SessionID,
			})
			msg := realtime.Message{Type: "presence", Payload: json.RawMessage(payload)}
			for _, friendID := range acceptedFriendPlayersInProject(ctx, d, callerID, projectID) {
				_ = d.Hub.Send(ctx, tenantID, friendID, msg)
			}
		}

		out := &presenceUpdateOutput{}
		out.Body.OK = true
		return out, nil
	})
}

// acceptedFriendPlayersInProject resolves the caller's account, lists their
// accepted friend accounts, and maps those accounts back to project_players in the
// given project. Best-effort: any error yields an empty slice (presence
// fan-out is not load-bearing). Anonymous callers have no account → no fan-out.
func acceptedFriendPlayersInProject(ctx context.Context, d Deps, callerID, projectID int64) []int64 {
	if projectID <= 0 {
		return nil
	}
	var out []int64
	if err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		myAcc, err := q.GetPlayerLinkedAccountID(ctx, callerID)
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
		euRows, err := q.ResolvePlayersForAccountsInProject(ctx, sqlcgen.ResolvePlayersForAccountsInProjectParams{
			ProjectID: projectID, AccountIds: friendAccts,
		})
		if err != nil {
			return err
		}
		for _, er := range euRows {
			out = append(out, er.PlayerID)
		}
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "presence fan-out: resolve friends", "err", err)
		return nil
	}
	return out
}
