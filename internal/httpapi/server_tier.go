package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// Server-tier routes: a dedicated game server authenticates with its secret
// API key alone and names the player it acts for by id. There is no player
// session, so every handler gates on requireServerTierPlayer before touching
// the player's data. Publishable keys never reach these handlers — the router
// binds them behind RequireKeyType(secret) plus an RBAC permission check.

var (
	errServerPlayerDisabled = errors.New("server tier: player disabled")
	errServerPlayerBanned   = errors.New("server tier: player banned")
)

// requireServerTierPlayer verifies the named player exists in the project and
// may still be acted for. pgx.ErrNoRows covers missing, soft-deleted, and
// sibling-project players alike; disabled and tenant-banned players are
// distinguished so a trusted game server learns why its write was refused.
func requireServerTierPlayer(ctx context.Context, q *sqlcgen.Queries, playerID, projectID int64) error {
	row, err := q.GetPlayerModerationState(ctx, sqlcgen.GetPlayerModerationStateParams{
		ID: playerID, ProjectID: projectID,
	})
	if err != nil {
		return err
	}
	if row.DisabledAt.Valid {
		return errServerPlayerDisabled
	}
	_, err = q.IsPlayerBannedByTenant(ctx, playerID)
	if err == nil {
		return errServerPlayerBanned
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// serverTierPlayerError maps requireServerTierPlayer failures onto the wire.
// A nil return means the error was not a gate outcome and the caller must
// handle it.
func serverTierPlayerError(err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return huma.Error404NotFound("player not found")
	case errors.Is(err, errServerPlayerDisabled):
		return huma.Error403Forbidden("player disabled")
	case errors.Is(err, errServerPlayerBanned):
		return huma.Error403Forbidden("player banned")
	}
	return nil
}

// gateServerTierPlayer runs the player gate in its own transaction on the
// primary (a just-created player must not 404 off a lagging replica). Used by
// the storage handlers, whose shared read/write helpers own their own
// transactions.
func gateServerTierPlayer(ctx context.Context, d Deps, playerID, projectID int64) error {
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		return requireServerTierPlayer(ctx, sqlcgen.New(tx), playerID, projectID)
	})
	if err == nil {
		return nil
	}
	if werr := serverTierPlayerError(err); werr != nil {
		return werr
	}
	return serverError(ctx, "server tier: player gate", err)
}

func serverProjectID(ctx context.Context) (int64, error) {
	projectID, ok := db.ProjectFromContext(ctx)
	if !ok {
		return 0, huma.Error400BadRequest("api key has no project pin")
	}
	return projectID, nil
}

type serverSubmitScoreRequest struct {
	PlayerID int64 `json:"player_id" minimum:"1" example:"42"`
	Score    int64 `json:"score" example:"1500"`
	// Optional JSON object stored with the score; same semantics as the
	// player-session submit route.
	Metadata json.RawMessage `json:"metadata,omitempty" example:"{\"ghost\":\"r-42\"}"`
}

type serverLeaderboardSubmitInput struct {
	ID   int64 `path:"id" minimum:"1" example:"1"`
	Body serverSubmitScoreRequest
}

func registerServerLeaderboardSubmit(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "serverSubmitScore",
		Method:        http.MethodPost,
		Path:          "/v1/server/leaderboards/{id}/scores",
		Summary:       "Server-tier: submit a score for a player",
		Tags:          []string{"Leaderboards"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusCreated,
	}, serverLeaderboardSubmit(d))
}

func serverLeaderboardSubmit(d Deps) func(context.Context, *serverLeaderboardSubmitInput) (*struct{}, error) {
	return func(ctx context.Context, in *serverLeaderboardSubmitInput) (*struct{}, error) {
		projectID, err := serverProjectID(ctx)
		if err != nil {
			return nil, err
		}
		if err := validateScoreMetadata(in.Body.Metadata); err != nil {
			return nil, err
		}
		err = submitScoreToBoard(ctx, d, in.ID, projectID, scoreSubmission{
			playerID: in.Body.PlayerID, score: in.Body.Score, metadata: in.Body.Metadata,
		}, func(q *sqlcgen.Queries) error {
			return requireServerTierPlayer(ctx, q, in.Body.PlayerID, projectID)
		})
		if werr := leaderboardSubmitError(err); werr != nil {
			return nil, werr
		}
		if werr := serverTierPlayerError(err); werr != nil {
			return nil, werr
		}
		if err != nil {
			return nil, serverError(ctx, "server leaderboard submit: tx", err)
		}
		return nil, nil
	}
}

type serverStoragePutInput struct {
	PlayerID int64  `path:"player_id" minimum:"1" example:"42"`
	Key      string `path:"key" example:"save-slot-1"`
	IfMatch  string `header:"If-Match" example:"7"`
	Body     json.RawMessage
}

type serverStorageKeyInput struct {
	PlayerID int64  `path:"player_id" minimum:"1" example:"42"`
	Key      string `path:"key" example:"save-slot-1"`
}

type serverStorageListInput struct {
	PlayerID  int64  `path:"player_id" minimum:"1" example:"42"`
	KeyPrefix string `query:"key_prefix" example:"save-"`
	Limit     string `query:"limit" example:"50"`
	Cursor    string `query:"cursor" example:"104"`
}

func registerServerStorageRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "serverListStorageObjects",
		Method:      http.MethodGet,
		Path:        "/v1/server/players/{player_id}/storage/objects",
		Summary:     "Server-tier: list a player's storage objects",
		Tags:        []string{"Cloud Saves"},
		Security:    apiKeySecurity,
	}, serverStorageList(d))

	huma.Register(api, huma.Operation{
		OperationID:  "serverPutStorageObject",
		Method:       http.MethodPut,
		Path:         "/v1/server/players/{player_id}/storage/objects/{key}",
		Summary:      "Server-tier: create or replace a player's storage object",
		Tags:         []string{"Cloud Saves"},
		Security:     apiKeySecurity,
		MaxBodyBytes: storageBodyReadLimit(d),
		RequestBody: &huma.RequestBody{
			Required: true,
			Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{
				Description: "Any JSON value to store: object, array, string, number, boolean, or null.",
				Examples:    []any{map[string]any{"level": 3, "hp": 100}},
			}}},
		},
	}, serverStoragePut(d))

	huma.Register(api, huma.Operation{
		OperationID: "serverGetStorageObject",
		Method:      http.MethodGet,
		Path:        "/v1/server/players/{player_id}/storage/objects/{key}",
		Summary:     "Server-tier: get a player's storage object",
		Tags:        []string{"Cloud Saves"},
		Security:    apiKeySecurity,
	}, serverStorageGet(d))
}

func serverStoragePut(d Deps) func(context.Context, *serverStoragePutInput) (*storageObjectOutput, error) {
	return func(ctx context.Context, in *serverStoragePutInput) (*storageObjectOutput, error) {
		projectID, err := serverProjectID(ctx)
		if err != nil {
			return nil, err
		}
		if err := gateServerTierPlayer(ctx, d, in.PlayerID, projectID); err != nil {
			return nil, err
		}
		return putStorageForOwner(ctx, d, projectID, in.PlayerID, &storagePutInput{
			Key: in.Key, IfMatch: in.IfMatch, Body: in.Body,
		})
	}
}

func serverStorageGet(d Deps) func(context.Context, *serverStorageKeyInput) (*storageObjectOutput, error) {
	return func(ctx context.Context, in *serverStorageKeyInput) (*storageObjectOutput, error) {
		projectID, err := serverProjectID(ctx)
		if err != nil {
			return nil, err
		}
		if err := gateServerTierPlayer(ctx, d, in.PlayerID, projectID); err != nil {
			return nil, err
		}
		return getStorageForOwner(ctx, d, projectID, in.PlayerID, in.Key)
	}
}

func serverStorageList(d Deps) func(context.Context, *serverStorageListInput) (*storageListOutput, error) {
	return func(ctx context.Context, in *serverStorageListInput) (*storageListOutput, error) {
		projectID, err := serverProjectID(ctx)
		if err != nil {
			return nil, err
		}
		if err := gateServerTierPlayer(ctx, d, in.PlayerID, projectID); err != nil {
			return nil, err
		}
		return listStorageForOwner(ctx, d, projectID, in.PlayerID, &storageListInput{
			KeyPrefix: in.KeyPrefix, Limit: in.Limit, Cursor: in.Cursor,
		})
	}
}
