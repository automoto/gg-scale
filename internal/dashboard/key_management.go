package dashboard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

var (
	errInvalidTenant      = errors.New("dashboard: tenant id is required")
	errInvalidProjectName = errors.New("dashboard: project name is required")
	errDuplicateProject   = errors.New("dashboard: project with that name already exists")
	errInvalidKeyType     = errors.New("dashboard: key type must be 'publishable' or 'secret'")
	errInvalidScope       = errors.New("dashboard: unknown feature scope")
	errKeyNotInTenant     = errors.New("dashboard: api key not found in tenant")
	errScopeNotGrantable  = errors.New("dashboard: feature is not enabled for this key")
)

type createKeyInput struct {
	TenantID  int64
	ProjectID *int64
	Label     string
	// KeyType is "publishable" (for keys embedded in shipped game clients)
	// or "secret" (for game servers / tenant backends). Required —
	// caller must validate against the tenant.KeyType* constants before
	// reaching createAPIKey.
	KeyType tenant.KeyType
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
		return nil, errors.New(msgDashboardPoolNeeded)
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
				Scopes:      row.Scopes,
			}
			if row.CreatedAt.Valid {
				key.CreatedAt = row.CreatedAt.Time
			}
			if row.RevokedAt.Valid {
				t := row.RevokedAt.Time
				key.RevokedAt = &t
			}
			key.FleetGrantable = h.scopeGrantable(ctx, tenantID, row.ProjectID, tenant.ScopeFleet)
			key.RelayGrantable = h.scopeGrantable(ctx, tenantID, row.ProjectID, tenant.ScopeP2PRelay)
			key.MatchmakerGrantable = h.scopeGrantable(ctx, tenantID, row.ProjectID, tenant.ScopeMatchmaker)
			out = append(out, key)
		}
		return nil
	})
	return out, err
}

// scopeGrantable reports whether a per-key feature scope can be granted: the
// startup env kill switch must be on AND a feature_grant row must enable the
// backing feature for the key's tenant/project. Keys pinned to no project use
// tenant-level grants (projectID 0).
func (h *Handler) scopeGrantable(ctx context.Context, tenantID int64, projectID *int64, scope string) bool {
	feature, ok := scopeFeature(scope)
	if !ok {
		return false
	}
	switch scope {
	case tenant.ScopeFleet:
		if !h.cfg.FleetEnabled {
			return false
		}
	case tenant.ScopeP2PRelay:
		if !h.cfg.RelayEnabled {
			return false
		}
	case tenant.ScopeMatchmaker:
		// No env kill switch: matchmaker is zero-config.
	}
	if h.rbac == nil {
		return false
	}
	var pid int64
	if projectID != nil {
		pid = *projectID
	}
	enabled, err := h.rbac.FeatureEnabled(ctx, tenantID, pid, feature)
	return err == nil && enabled
}

// scopeFeature maps a per-key scope to the feature_grant gate that governs it.
func scopeFeature(scope string) (rbac.Feature, bool) {
	switch scope {
	case tenant.ScopeFleet:
		return rbac.FeatureDedicatedServers, true
	case tenant.ScopeP2PRelay:
		return rbac.FeatureP2PRelay, true
	case tenant.ScopeMatchmaker:
		return rbac.FeatureMatchmaker, true
	default:
		return "", false
	}
}

// setAPIKeyScope grants or revokes a single feature scope on a key. Granting
// re-validates that the feature is enabled so a scope can never outlive its
// feature_grant / env switch; revoking is always allowed.
func (h *Handler) setAPIKeyScope(ctx context.Context, actorID, tenantID, apiKeyID int64, scope string, grant bool) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	if _, ok := scopeFeature(scope); !ok {
		return errInvalidScope
	}
	ctx = db.WithTenant(ctx, tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetAPIKeyScopes(ctx, apiKeyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errKeyNotInTenant
		}
		if err != nil {
			return fmt.Errorf("get api key scopes: %w", err)
		}
		if grant && !h.scopeGrantable(ctx, tenantID, row.ProjectID, scope) {
			return errScopeNotGrantable
		}
		next := applyScope(row.Scopes, scope, grant)
		if err := q.SetAPIKeyScopes(ctx, sqlcgen.SetAPIKeyScopesParams{ID: apiKeyID, Scopes: next}); err != nil {
			return fmt.Errorf("set api key scopes: %w", err)
		}
		action := "dashboard.api_key.scope_revoke"
		if grant {
			action = "dashboard.api_key.scope_grant"
		}
		return auditlog.WritePlatform(ctx, tx, actorID, action, strconv.FormatInt(apiKeyID, 10), map[string]any{
			"scope":     scope,
			"tenant_id": tenantID,
		})
	}); err != nil {
		return err
	}
	if h.cache != nil {
		// Drop any cached rate-limit bucket so the middleware re-reads the key
		// (and its refreshed scopes) on the next request.
		_ = h.cache.Delete(ctx, ratelimit.APIKeyBucketKey(apiKeyID))
	}
	return nil
}

// applyScope returns scopes with scope added (grant) or removed (revoke),
// preserving order and de-duplicating.
func applyScope(scopes []string, scope string, grant bool) []string {
	out := make([]string, 0, len(scopes)+1)
	seen := false
	for _, s := range scopes {
		if s == scope {
			seen = true
			if !grant {
				continue
			}
		}
		out = append(out, s)
	}
	if grant && !seen {
		out = append(out, scope)
	}
	return out
}

func (h *Handler) listProjects(ctx context.Context, tenantID int64) ([]ProjectOption, error) {
	if tenantID <= 0 {
		return nil, errInvalidTenant
	}
	if h.pool == nil {
		return nil, errors.New(msgDashboardPoolNeeded)
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
			opt := ProjectOption{ID: row.ID, Name: row.Name, PublicJoiningEnabled: row.PublicJoiningEnabled}
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
		return ProjectOption{}, errors.New(msgDashboardPoolNeeded)
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

func (h *Handler) createAPIKey(ctx context.Context, actorID int64, in createKeyInput) (createKeyResult, error) {
	if in.TenantID <= 0 {
		return createKeyResult{}, errInvalidTenant
	}
	if in.KeyType != tenant.KeyTypePublishable && in.KeyType != tenant.KeyTypeSecret {
		return createKeyResult{}, errInvalidKeyType
	}
	if h.pool == nil {
		return createKeyResult{}, errors.New(msgDashboardPoolNeeded)
	}

	apiKey, err := randomAPIKey(in.KeyType)
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
			KeyType:   string(in.KeyType),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errProjectNotInTenant
			}
			return fmt.Errorf("create api key: %w", err)
		}
		if h.rbac != nil {
			if err := h.rbac.AddAPIKeyRoleTx(ctx, tx, row.ID, in.TenantID, in.KeyType); err != nil {
				return fmt.Errorf("rbac api key create: %w", err)
			}
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "dashboard.api_key.create", strconv.FormatInt(row.ID, 10), map[string]any{
			"label":      in.Label,
			"project_id": in.ProjectID,
			"tenant_id":  in.TenantID,
			"key_type":   string(in.KeyType),
		})
	})
	if err != nil {
		return createKeyResult{}, err
	}
	h.reloadRBACPolicy(ctx)
	return createKeyResult{APIKeyID: row.ID, APIKey: apiKey}, nil
}

func (h *Handler) updateAPIKeyLabel(ctx context.Context, actorID, tenantID, apiKeyID int64, label string) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		if err := sqlcgen.New(tx).UpdateAPIKeyLabel(ctx, sqlcgen.UpdateAPIKeyLabelParams{
			ID:    apiKeyID,
			Label: strings.TrimSpace(label),
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "dashboard.api_key.relabel", strconv.FormatInt(apiKeyID, 10), map[string]any{"label": label, "tenant_id": tenantID})
	})
}

func (h *Handler) revokeAPIKey(ctx context.Context, actorID, tenantID, apiKeyID int64) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	ctx = db.WithTenant(ctx, tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		if err := sqlcgen.New(tx).RevokeAPIKey(ctx, apiKeyID); err != nil {
			return err
		}
		if h.rbac != nil {
			if err := h.rbac.RemoveAPIKeyRolesTx(ctx, tx, apiKeyID); err != nil {
				return fmt.Errorf("rbac api key revoke: %w", err)
			}
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "dashboard.api_key.revoke", strconv.FormatInt(apiKeyID, 10), map[string]any{"tenant_id": tenantID})
	}); err != nil {
		return err
	}
	h.reloadRBACPolicy(ctx)
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
