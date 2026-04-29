package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/enduser"
)

const storageListMaxLimit = 100

type storageObjectResponse struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Version   int64           `json:"version"`
	UpdatedAt string          `json:"updated_at"`
}

// PUT /v1/storage/objects/{key} — m1.md 4.2.1.
func storagePutHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		if key == "" {
			http.Error(w, "key required", http.StatusBadRequest)
			return
		}

		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "body too large or unreadable", http.StatusBadRequest)
			return
		}
		if !json.Valid(raw) {
			http.Error(w, "value must be valid JSON", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "no project", http.StatusBadRequest)
			return
		}
		ownerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		ifMatch := r.Header.Get("If-Match")
		var (
			version   int64
			updatedAt time.Time
		)
		err = d.Pool.Q(ctx, func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)
			if ifMatch != "" {
				expected, perr := strconv.ParseInt(ifMatch, 10, 64)
				if perr != nil {
					return errIfMatchInvalid
				}
				row, qerr := q.PutStorageObjectIfMatch(ctx, sqlcgen.PutStorageObjectIfMatchParams{
					ProjectID: projectID, OwnerUserID: ownerID, Key: key,
					Version: expected, Value: raw,
				})
				if qerr != nil {
					return qerr
				}
				version = row.Version
				updatedAt = row.UpdatedAt.Time
				return nil
			}
			row, qerr := q.PutStorageObject(ctx, sqlcgen.PutStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: key, Value: raw,
			})
			if qerr != nil {
				return qerr
			}
			version = row.Version
			updatedAt = row.UpdatedAt.Time
			return nil
		})
		if errors.Is(err, errIfMatchInvalid) {
			http.Error(w, "If-Match must be an integer version", http.StatusBadRequest)
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "version mismatch", http.StatusPreconditionFailed)
			return
		}
		if err != nil {
			internalError(w, "storage put: tx", err)
			return
		}

		writeJSON(w, storageObjectResponse{
			Key: key, Value: raw, Version: version,
			UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
		})
	}
}

// GET /v1/storage/objects/{key} — m1.md 4.2.2.
func storageGetHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		ctx := r.Context()
		projectID, _ := db.ProjectFromContext(ctx)
		ownerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		var resp storageObjectResponse
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			row, qerr := sqlcgen.New(tx).GetStorageObject(ctx, sqlcgen.GetStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: key,
			})
			if qerr != nil {
				return qerr
			}
			resp = storageObjectResponse{
				Key: key, Value: row.Value, Version: row.Version,
				UpdatedAt: row.UpdatedAt.Time.UTC().Format(time.RFC3339),
			}
			return nil
		})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			internalError(w, "storage get: tx", err)
			return
		}
		writeJSON(w, resp)
	}
}

// DELETE /v1/storage/objects/{key} — m1.md 4.2.3.
func storageDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "key")
		ctx := r.Context()
		projectID, _ := db.ProjectFromContext(ctx)
		ownerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			return sqlcgen.New(tx).SoftDeleteStorageObject(ctx, sqlcgen.SoftDeleteStorageObjectParams{
				ProjectID: projectID, OwnerUserID: ownerID, Key: key,
			})
		})
		if err != nil {
			internalError(w, "storage delete: tx", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// GET /v1/storage/objects — m1.md 4.2.4 (cursor pagination).
func storageListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		projectID, _ := db.ProjectFromContext(ctx)
		ownerID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
			return
		}

		prefix := r.URL.Query().Get("key_prefix")
		limit := parseLimit(r.URL.Query().Get("limit"), 50, storageListMaxLimit)
		cursor := parseCursor(r.URL.Query().Get("cursor"))

		var items []storageObjectResponse
		var lastID int64
		err := d.Pool.Q(ctx, func(tx pgx.Tx) error {
			rows, qerr := sqlcgen.New(tx).ListStorageObjects(ctx, sqlcgen.ListStorageObjectsParams{
				ProjectID: projectID, OwnerUserID: ownerID,
				Column3: prefix, ID: cursor, Limit: limit,
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
			internalError(w, "storage list: tx", err)
			return
		}
		var next string
		if len(items) == int(limit) {
			next = strconv.FormatInt(lastID, 10)
		}
		writeJSON(w, map[string]any{
			"items":       items,
			"next_cursor": next,
		})
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
