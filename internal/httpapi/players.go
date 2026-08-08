package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/playerauth"
)

// playerResolveMaxIDs caps the batch resolve endpoint's ids list.
const playerResolveMaxIDs = 100

type publicPlayerResponse struct {
	ID          int64  `json:"id" example:"42"`
	DisplayName string `json:"display_name,omitempty" example:"Nova Fox"`
	CreatedAt   string `json:"created_at" example:"2026-01-02T15:04:05Z"`
}

type playerGetInput struct {
	ID int64 `path:"id" minimum:"1" example:"42"`
}

type playerGetOutput struct {
	Body publicPlayerResponse
}

type playersResolveInput struct {
	IDs string `query:"ids" example:"42,87,101"`
}

type playersResolveResult struct {
	Players []publicPlayerResponse `json:"players"`
}

type playersResolveOutput struct {
	Body playersResolveResult
}

func registerPlayerLookupRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getPlayer",
		Method:      http.MethodGet,
		Path:        "/v1/players/{id}",
		Summary:     "Get a player's public profile",
		Description: "Public fields only — never the account email. Project-scoped: " +
			"an id from another project returns 404.",
		Tags:     []string{"Player Profiles"},
		Security: playerSecurity,
		// The hosted account pages live under /v1/players/account/. Constrain
		// the param to digits so those URLs keep falling through to that
		// mount instead of matching this route.
		Metadata: map[string]any{chiPathMetadata: "/v1/players/{id:[0-9]+}"},
	}, playerGet(d))

	huma.Register(api, huma.Operation{
		OperationID: "resolvePlayers",
		Method:      http.MethodGet,
		Path:        "/v1/players",
		Summary:     "Resolve player ids to public profiles",
		Description: fmt.Sprintf("Batch lookup of up to %d comma-separated ids. "+
			"Unknown ids and ids from other projects are omitted from the result.",
			playerResolveMaxIDs),
		Tags:     []string{"Player Profiles"},
		Security: playerSecurity,
	}, playersResolve(d))
}

func publicPlayer(id int64, displayName *string, createdAt pgtype.Timestamptz) publicPlayerResponse {
	p := publicPlayerResponse{ID: id, CreatedAt: createdAt.Time.UTC().Format(time.RFC3339)}
	if displayName != nil {
		p.DisplayName = *displayName
	}
	return p
}

func playerGet(d Deps) func(context.Context, *playerGetInput) (*playerGetOutput, error) {
	return func(ctx context.Context, in *playerGetInput) (*playerGetOutput, error) {
		if _, ok := playerauth.IDFromContext(ctx); !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		// Project scope comes from the API-key pin, matching every sibling
		// player endpoint (game sessions, storage, leaderboards).
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}

		var resp publicPlayerResponse
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetPublicPlayer(ctx, sqlcgen.GetPublicPlayerParams{
				ID: in.ID, ProjectID: projectID,
			})
			if qerr != nil {
				return qerr
			}
			resp = publicPlayer(row.ID, row.DisplayName, row.CreatedAt)
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("player not found")
		}
		if err != nil {
			return nil, serverError(ctx, "player get", err)
		}
		return &playerGetOutput{Body: resp}, nil
	}
}

func playersResolve(d Deps) func(context.Context, *playersResolveInput) (*playersResolveOutput, error) {
	return func(ctx context.Context, in *playersResolveInput) (*playersResolveOutput, error) {
		if _, ok := playerauth.IDFromContext(ctx); !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("api key has no project pin")
		}

		ids, perr := parsePlayerIDs(in.IDs)
		if perr != nil {
			return nil, huma.Error400BadRequest(perr.Error())
		}

		players := make([]publicPlayerResponse, 0, len(ids))
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListPublicPlayers(ctx, sqlcgen.ListPublicPlayersParams{
				Ids: ids, ProjectID: projectID,
			})
			if qerr != nil {
				return qerr
			}
			for _, row := range rows {
				players = append(players, publicPlayer(row.ID, row.DisplayName, row.CreatedAt))
			}
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "players resolve", err)
		}
		return &playersResolveOutput{Body: playersResolveResult{Players: players}}, nil
	}
}

func parsePlayerIDs(raw string) ([]int64, error) {
	// Empty segments (a trailing or doubled comma from a programmatically
	// built list) are skipped, not errors.
	parts := make([]string, 0)
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("ids required")
	}
	if len(parts) > playerResolveMaxIDs {
		return nil, fmt.Errorf("too many ids (max %d)", playerResolveMaxIDs)
	}
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id < 1 {
			return nil, errors.New("ids must be positive integers")
		}
		out = append(out, id)
	}
	return out, nil
}
