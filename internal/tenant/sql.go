package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
)

// NewSQLLookup builds a Lookup that resolves api_keys against pool. The
// query runs without a tenant GUC set, which is allowed by the bootstrap
// policy on api_keys (see migration 0010).
func NewSQLLookup(pool *pgxpool.Pool) Lookup {
	q := sqlcgen.New(pool)
	return func(ctx context.Context, keyHash []byte) (*APIKey, error) {
		row, err := q.GetAPIKeyByHash(ctx, keyHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnknownKey
		}
		if err != nil {
			return nil, fmt.Errorf("api_keys lookup: %w", err)
		}
		return &APIKey{
			ID:        row.ID,
			TenantID:  row.TenantID,
			ProjectID: row.ProjectID,
			Tier:      Tier(row.Tier),
			Type:      KeyType(row.KeyType),
			Revoked:   row.RevokedAt.Valid,
		}, nil
	}
}
