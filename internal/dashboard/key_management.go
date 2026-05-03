package dashboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/ratelimit"
)

var errInvalidTenant = errors.New("dashboard: tenant id is required")

type createKeyInput struct {
	TenantID  int64
	ProjectID *int64
	Label     string
}

type createKeyResult struct {
	APIKeyID int64
	APIKey   string
}

func (h *Handler) listAPIKeys(ctx context.Context, tenantID int64) ([]APIKeyView, error) {
	if tenantID <= 0 {
		return nil, errInvalidTenant
	}
	if h.pool == nil {
		return nil, errors.New("dashboard: database pool is required")
	}

	var out []APIKeyView
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListAPIKeys(ctx)
		if err != nil {
			return fmt.Errorf("list api keys: %w", err)
		}
		out = make([]APIKeyView, 0, len(rows))
		for _, row := range rows {
			key := APIKeyView{
				ID:        row.ID,
				ProjectID: "-",
				Label:     stringValue(row.Label),
			}
			if row.ProjectID != nil {
				key.ProjectID = strconv.FormatInt(*row.ProjectID, 10)
			}
			if row.CreatedAt.Valid {
				key.CreatedAt = row.CreatedAt.Time
			}
			if row.RevokedAt.Valid {
				t := row.RevokedAt.Time
				key.RevokedAt = &t
			}
			out = append(out, key)
		}
		return nil
	})
	return out, err
}

func (h *Handler) createAPIKey(ctx context.Context, in createKeyInput) (createKeyResult, error) {
	if in.TenantID <= 0 {
		return createKeyResult{}, errInvalidTenant
	}
	if h.pool == nil {
		return createKeyResult{}, errors.New("dashboard: database pool is required")
	}

	apiKey, err := randomAPIKey()
	if err != nil {
		return createKeyResult{}, err
	}
	sum := sha256.Sum256([]byte(apiKey))

	var row sqlcgen.CreateDashboardAPIKeyRow
	ctx = db.WithTenant(ctx, in.TenantID)
	err = h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).CreateDashboardAPIKey(ctx, sqlcgen.CreateDashboardAPIKeyParams{
			ProjectID: in.ProjectID,
			KeyHash:   sum[:],
			Label:     strings.TrimSpace(in.Label),
		})
		if err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return createKeyResult{}, err
	}
	return createKeyResult{APIKeyID: row.ID, APIKey: apiKey}, nil
}

func (h *Handler) updateAPIKeyLabel(ctx context.Context, tenantID, apiKeyID int64, label string) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).UpdateAPIKeyLabel(ctx, sqlcgen.UpdateAPIKeyLabelParams{
			ID:    apiKeyID,
			Label: strings.TrimSpace(label),
		})
	})
}

func (h *Handler) revokeAPIKey(ctx context.Context, tenantID, apiKeyID int64) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	ctx = db.WithTenant(ctx, tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).RevokeAPIKey(ctx, apiKeyID)
	}); err != nil {
		return err
	}
	if h.cache == nil {
		return nil
	}
	return h.cache.Delete(ctx, ratelimit.APIKeyBucketKey(apiKeyID))
}

func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
