package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/storagelimit"
)

const storageListMaxLimit = 100

// storageLimit returns the platform default value-size cap from config, or the
// compiled fallback when config is unset. Oversize writes → 413.
func storageLimit(d Deps) int64 {
	if d.StorageMaxValueBytes > 0 {
		return d.StorageMaxValueBytes
	}
	return storagelimit.DefaultMaxValueBytes
}

type storageObjectResponse struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Version   int64           `json:"version"`
	UpdatedAt string          `json:"updated_at"`
}

type storageObjectOutput struct {
	Body storageObjectResponse
}

type storagePutInput struct {
	Key     string `path:"key"`
	IfMatch string `header:"If-Match"`
	RawBody []byte
}

type storageKeyInput struct {
	Key string `path:"key"`
}

type storageListInput struct {
	KeyPrefix string `query:"key_prefix"`
	Limit     string `query:"limit"`
	Cursor    string `query:"cursor"`
}

type storageListResult struct {
	Items      []storageObjectResponse `json:"items"`
	NextCursor string                  `json:"next_cursor"`
}

type storageListOutput struct {
	Body storageListResult
}

func registerStorageRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "listStorageObjects",
		Method:      http.MethodGet,
		Path:        "/v1/storage/objects",
		Summary:     "List the caller's storage objects",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, storageList(d))

	huma.Register(api, huma.Operation{
		OperationID: "putStorageObject",
		Method:      http.MethodPut,
		Path:        "/v1/storage/objects/{key}",
		Summary:     "Create or replace a storage object",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, storagePut(d))

	huma.Register(api, huma.Operation{
		OperationID: "getStorageObject",
		Method:      http.MethodGet,
		Path:        "/v1/storage/objects/{key}",
		Summary:     "Get a storage object",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, storageGet(d))

	huma.Register(api, huma.Operation{
		OperationID:   "deleteStorageObject",
		Method:        http.MethodDelete,
		Path:          "/v1/storage/objects/{key}",
		Summary:       "Delete a storage object",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, storageDelete(d))
}

func storagePut(d Deps) func(context.Context, *storagePutInput) (*storageObjectOutput, error) {
	return func(ctx context.Context, in *storagePutInput) (*storageObjectOutput, error) {
		if in.Key == "" {
			return nil, huma.Error400BadRequest("key required")
		}
		if !json.Valid(in.RawBody) {
			return nil, huma.Error400BadRequest("value must be valid JSON")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("no project")
		}
		ownerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		limit, err := resolveStorageLimit(ctx, d, projectID)
		if err != nil {
			return nil, serverError(ctx, "storage put: resolve limit", err)
		}
		if int64(len(in.RawBody)) > limit {
			return nil, huma.Error413RequestEntityTooLarge("value exceeds maximum size")
		}

		version, updatedAt, err := putStorageObject(ctx, d, projectID, ownerID, in)
		switch {
		case errors.Is(err, errIfMatchInvalid):
			return nil, huma.Error400BadRequest("If-Match must be an integer version")
		case errors.Is(err, pgx.ErrNoRows):
			return nil, huma.Error412PreconditionFailed("version mismatch")
		case err != nil:
			return nil, serverError(ctx, "storage put: tx", err)
		}

		return &storageObjectOutput{Body: storageObjectResponse{
			Key: in.Key, Value: in.RawBody, Version: version,
			UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
		}}, nil
	}
}

// resolveStorageLimit returns the effective value-size cap: the platform
// default, unless a tenant/project override is configured.
func resolveStorageLimit(ctx context.Context, d Deps, projectID int64) (int64, error) {
	limit := storageLimit(d)
	if d.StorageLimits == nil {
		return limit, nil
	}
	tenantID, _ := db.TenantFromContext(ctx)
	return d.StorageLimits.Resolve(ctx, tenantID, projectID, limit)
}

// putStorageObject upserts the object in a transaction, honoring an optional
// If-Match version precondition, and returns the new version and timestamp.
func putStorageObject(ctx context.Context, d Deps, projectID, ownerID int64, in *storagePutInput) (int64, time.Time, error) {
	var (
		version   int64
		updatedAt time.Time
	)
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if in.IfMatch == "" {
			row, qerr := q.PutStorageObject(ctx, sqlcgen.PutStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key, Value: in.RawBody,
			})
			if qerr != nil {
				return qerr
			}
			version, updatedAt = row.Version, row.UpdatedAt.Time
			return nil
		}
		expected, perr := strconv.ParseInt(in.IfMatch, 10, 64)
		if perr != nil {
			return errIfMatchInvalid
		}
		row, qerr := q.PutStorageObjectIfMatch(ctx, sqlcgen.PutStorageObjectIfMatchParams{
			ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
			Version: expected, Value: in.RawBody,
		})
		if qerr != nil {
			return qerr
		}
		version, updatedAt = row.Version, row.UpdatedAt.Time
		return nil
	})
	return version, updatedAt, err
}

func storageGet(d Deps) func(context.Context, *storageKeyInput) (*storageObjectOutput, error) {
	return func(ctx context.Context, in *storageKeyInput) (*storageObjectOutput, error) {
		projectID, projectOK := db.ProjectFromContext(ctx)
		if !projectOK {
			return nil, huma.Error400BadRequest("project pin required")
		}
		ownerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		var resp storageObjectResponse
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetStorageObject(ctx, sqlcgen.GetStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
			})
			if qerr != nil {
				return qerr
			}
			resp = storageObjectResponse{
				Key: in.Key, Value: row.Value, Version: row.Version,
				UpdatedAt: row.UpdatedAt.Time.UTC().Format(time.RFC3339),
			}
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("not found")
		}
		if err != nil {
			return nil, serverError(ctx, "storage get: tx", err)
		}
		return &storageObjectOutput{Body: resp}, nil
	}
}

func storageDelete(d Deps) func(context.Context, *storageKeyInput) (*struct{}, error) {
	return func(ctx context.Context, in *storageKeyInput) (*struct{}, error) {
		projectID, projectOK := db.ProjectFromContext(ctx)
		if !projectOK {
			return nil, huma.Error400BadRequest("project pin required")
		}
		ownerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SoftDeleteStorageObject(ctx, sqlcgen.SoftDeleteStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
			})
		})
		if err != nil {
			return nil, serverError(ctx, "storage delete: tx", err)
		}
		return nil, nil
	}
}

func storageList(d Deps) func(context.Context, *storageListInput) (*storageListOutput, error) {
	return func(ctx context.Context, in *storageListInput) (*storageListOutput, error) {
		projectID, projectOK := db.ProjectFromContext(ctx)
		if !projectOK {
			return nil, huma.Error400BadRequest("project pin required")
		}
		ownerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}

		limit := parseLimit(in.Limit, 50, storageListMaxLimit)
		cursor := parseCursor(in.Cursor)

		var items []storageObjectResponse
		var lastID int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListStorageObjects(ctx, sqlcgen.ListStorageObjectsParams{
				ProjectID: projectID, OwnerUserID: ownerID,
				Column3: in.KeyPrefix, ID: cursor, Limit: limit,
			})
			if qerr != nil {
				return qerr
			}
			for _, row := range rows {
				items = append(items, storageObjectResponse{
					Key: row.Key, Value: row.Value, Version: row.Version,
					UpdatedAt: row.UpdatedAt.Time.UTC().Format(time.RFC3339),
				})
				lastID = row.ID
			}
			return nil
		})
		if err != nil {
			return nil, serverError(ctx, "storage list: tx", err)
		}
		var next string
		if len(items) == int(limit) {
			next = strconv.FormatInt(lastID, 10)
		}
		return &storageListOutput{Body: storageListResult{Items: items, NextCursor: next}}, nil
	}
}

func parseLimit(s string, def, max int32) int32 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n <= 0 {
		return def
	}
	if int32(n) > max {
		return max
	}
	return int32(n)
}

func parseCursor(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var errIfMatchInvalid = errors.New("storage: If-Match invalid")
