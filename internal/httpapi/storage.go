package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/storagelimit"
	"github.com/ggscale/ggscale/internal/tenant"
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

// Huma rejects a request after reading exactly MaxBodyBytes, while the storage
// limit is inclusive. Give the reader one extra byte so the handler can accept
// an exact-limit value and reject anything larger.
func storageBodyReadLimit(d Deps) int64 {
	limit := storageLimit(d)
	if limit == math.MaxInt64 {
		return limit
	}
	return limit + 1
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

type storageObjectListItemResponse struct {
	Key       string `json:"key"`
	Version   int64  `json:"version"`
	UpdatedAt string `json:"updated_at"`
	SizeBytes int64  `json:"size_bytes"`
}

type storagePutInput struct {
	Key     string `path:"key"`
	IfMatch string `header:"If-Match"`
	Body    json.RawMessage
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
	Items      []storageObjectListItemResponse `json:"items"`
	NextCursor string                          `json:"next_cursor"`
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
		OperationID:  "putStorageObject",
		Method:       http.MethodPut,
		Path:         "/v1/storage/objects/{key}",
		Summary:      "Create or replace a storage object",
		Tags:         []string{"/v1"},
		Security:     playerSecurity,
		MaxBodyBytes: storageBodyReadLimit(d),
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
		if !json.Valid(in.Body) {
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
		if int64(len(in.Body)) > limit {
			return nil, huma.Error413RequestEntityTooLarge("value exceeds maximum size")
		}

		version, updatedAt, err := putStorageObject(ctx, d, projectID, ownerID, in)
		var qe *quota.ErrQuotaExceeded
		switch {
		case errors.Is(err, errIfMatchInvalid):
			return nil, huma.Error400BadRequest("If-Match must be an integer version")
		case errors.Is(err, pgx.ErrNoRows):
			return nil, huma.Error412PreconditionFailed("version mismatch")
		case errors.As(err, &qe):
			d.Metrics.QuotaRejection(qe.Axis)
			return nil, huma.Error403Forbidden(fmt.Sprintf("storage quota exceeded: tenant storage limit is %d bytes", qe.Limit))
		case err != nil:
			return nil, serverError(ctx, "storage put: tx", err)
		}

		return &storageObjectOutput{Body: storageObjectResponse{
			Key: in.Key, Value: in.Body, Version: version,
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
// If-Match version precondition, and returns the new version and timestamp. It
// enforces the tenant storage quota before writing and adjusts the tenant's
// byte counter in the same tx so the counter cannot drift from the object rows.
func putStorageObject(ctx context.Context, d Deps, projectID, ownerID int64, in *storagePutInput) (int64, time.Time, error) {
	var (
		version   int64
		updatedAt time.Time
	)
	err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if lerr := q.LockStorageObjectForWrite(ctx, sqlcgen.LockStorageObjectForWriteParams{
			ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
		}); lerr != nil {
			return fmt.Errorf("storage write lock: %w", lerr)
		}
		delta, qerr := storageWriteDelta(ctx, q, projectID, ownerID, in.Key, in.Body)
		if qerr != nil {
			return qerr
		}
		if in.IfMatch == "" {
			row, err := q.PutStorageObject(ctx, sqlcgen.PutStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key, Value: in.Body,
			})
			if err != nil {
				return err
			}
			version, updatedAt = row.Version, row.UpdatedAt.Time
		} else {
			expected, perr := strconv.ParseInt(in.IfMatch, 10, 64)
			if perr != nil {
				return errIfMatchInvalid
			}
			row, err := q.PutStorageObjectIfMatch(ctx, sqlcgen.PutStorageObjectIfMatchParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
				Version: expected, Value: in.Body,
			})
			if err != nil {
				return err
			}
			version, updatedAt = row.Version, row.UpdatedAt.Time
		}
		return q.ApplyTenantStorageDelta(ctx, delta)
	})
	return version, updatedAt, err
}

// storageWriteDelta returns the byte delta a pending write will apply to the
// tenant storage counter (new size − old size). When the tenant enforces
// quotas it also rejects a growing write that would exceed the class limit;
// reads, deletes, and shrinking overwrites (delta<=0) always pass. The
// per-value max_value_bytes cap in storagePut is orthogonal (abuse guard).
func storageWriteDelta(ctx context.Context, q *sqlcgen.Queries, projectID, ownerID int64, key string, value []byte) (int64, error) {
	qc, err := q.GetTenantQuotaContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("tenant quota context: %w", err)
	}
	u, err := q.StorageUsageForWrite(ctx, sqlcgen.StorageUsageForWriteParams{
		ProjectID: projectID, OwnerUserID: ownerID, Key: key, NewValue: value,
	})
	if err != nil {
		return 0, fmt.Errorf("storage usage for write: %w", err)
	}
	delta := u.NewBytes - u.OldBytes
	if qc.EnforceQuotas {
		if err := quota.LimitsForClass(tenant.ClampTier(int(qc.Tier))).CheckStorage(u.TotalBytes, delta); err != nil {
			return 0, err
		}
	}
	return delta, nil
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
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
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
			q := sqlcgen.New(tx)
			if lerr := q.LockStorageObjectForWrite(ctx, sqlcgen.LockStorageObjectForWriteParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
			}); lerr != nil {
				return fmt.Errorf("storage write lock: %w", lerr)
			}
			freed, derr := q.SoftDeleteStorageObject(ctx, sqlcgen.SoftDeleteStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: in.Key,
			})
			if errors.Is(derr, pgx.ErrNoRows) {
				return nil // already absent; delete is idempotent
			}
			if derr != nil {
				return derr
			}
			return q.ApplyTenantStorageDelta(ctx, -freed)
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

		var items []storageObjectListItemResponse
		var lastID int64
		err := d.ReadPool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListStorageObjects(ctx, sqlcgen.ListStorageObjectsParams{
				ProjectID: projectID, OwnerUserID: ownerID,
				KeyPrefix: escapeStorageKeyPrefix(in.KeyPrefix), CursorID: cursor, RowLimit: limit,
			})
			if qerr != nil {
				return qerr
			}
			for _, row := range rows {
				items = append(items, storageObjectListItemResponse{
					Key: row.Key, Version: row.Version, SizeBytes: row.SizeBytes,
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

var storageKeyPrefixEscaper = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

func escapeStorageKeyPrefix(prefix string) string {
	return storageKeyPrefixEscaper.Replace(prefix)
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
