package dashboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/ratelimit"
)

var (
	errInvalidTenant      = errors.New("dashboard: tenant id is required")
	errInvalidProjectName = errors.New("dashboard: project name is required")
	errDuplicateProject   = errors.New("dashboard: project with that name already exists")
)

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
				ID:          row.ID,
				ProjectID:   row.ProjectID,
				ProjectName: stringValue(row.ProjectName),
				Label:       stringValue(row.Label),
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

func (h *Handler) listProjects(ctx context.Context, tenantID int64) ([]ProjectOption, error) {
	if tenantID <= 0 {
		return nil, errInvalidTenant
	}
	if h.pool == nil {
		return nil, errors.New("dashboard: database pool is required")
	}
	var out []ProjectOption
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListProjectsForTenant(ctx)
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		out = make([]ProjectOption, 0, len(rows))
		for _, row := range rows {
			opt := ProjectOption{ID: row.ID, Name: row.Name}
			if row.CreatedAt.Valid {
				opt.CreatedAt = row.CreatedAt.Time
			}
			out = append(out, opt)
		}
		return nil
	})
	return out, err
}

func (h *Handler) createProject(ctx context.Context, tenantID int64, name string) (ProjectOption, error) {
	if tenantID <= 0 {
		return ProjectOption{}, errInvalidTenant
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ProjectOption{}, errInvalidProjectName
	}
	if h.pool == nil {
		return ProjectOption{}, errors.New("dashboard: database pool is required")
	}
	var out ProjectOption
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).CreateProjectForTenant(ctx, name)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return errDuplicateProject
			}
			return fmt.Errorf("create project: %w", err)
		}
		out = ProjectOption{ID: row.ID, Name: row.Name}
		if row.CreatedAt.Valid {
			out.CreatedAt = row.CreatedAt.Time
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
