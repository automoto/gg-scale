package controlpanel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

var (
	errInvalidTenant      = errors.New("control panel: tenant id is required")
	errInvalidProjectName = errors.New("control panel: project name is required")
	errDuplicateProject   = errors.New("control panel: project with that name already exists")
	errInvalidKeyType     = errors.New("control panel: key type must be 'publishable' or 'secret'")
	errInvalidScope       = errors.New("control panel: unknown feature scope")
	errKeyNotInTenant     = errors.New("control panel: api key not found in tenant")
	errAPIKeyManageDenied = errors.New("control panel: api key management forbidden")
	errScopeNotGrantable  = errors.New("control panel: feature is not enabled for this key")
)

var managedAPIKeyScopes = []string{
	tenant.ScopeMatchmaker,
	tenant.ScopeFleet,
	tenant.ScopeP2PRelay,
}

type scopeChange struct {
	Scope string
	Grant bool
}

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
		return nil, errors.New(msgControlPanelPoolNeeded)
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

func parseManagedAPIKeyScopes(form url.Values) ([]string, error) {
	values := form["scopes"]
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, scope := range values {
		if !isManagedAPIKeyScope(scope) {
			return nil, errInvalidScope
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	return out, nil
}

func managedScopeChanges(current, desired []string) []scopeChange {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, scope := range desired {
		if isManagedAPIKeyScope(scope) {
			desiredSet[scope] = struct{}{}
		}
	}
	changes := make([]scopeChange, 0, len(managedAPIKeyScopes))
	for _, scope := range managedAPIKeyScopes {
		hasCurrent := slices.Contains(current, scope)
		_, wants := desiredSet[scope]
		if hasCurrent == wants {
			continue
		}
		changes = append(changes, scopeChange{Scope: scope, Grant: wants})
	}
	return changes
}

func applyManagedScopes(current, desired []string) []string {
	next := make([]string, 0, len(current)+len(desired))
	desiredSet := make(map[string]struct{}, len(desired))
	for _, scope := range desired {
		if isManagedAPIKeyScope(scope) {
			desiredSet[scope] = struct{}{}
		}
	}
	for _, scope := range current {
		if isManagedAPIKeyScope(scope) {
			continue
		}
		if !slices.Contains(next, scope) {
			next = append(next, scope)
		}
	}
	for _, scope := range managedAPIKeyScopes {
		if _, ok := desiredSet[scope]; ok {
			next = append(next, scope)
		}
	}
	return next
}

func isManagedAPIKeyScope(scope string) bool {
	for _, managed := range managedAPIKeyScopes {
		if scope == managed {
			return true
		}
	}
	return false
}

func apiKeyObjectForType(keyType tenant.KeyType) (string, bool) {
	switch keyType {
	case tenant.KeyTypePublishable:
		return rbac.ObjectAPIKeyPublic, true
	case tenant.KeyTypeSecret:
		return rbac.ObjectAPIKeySecret, true
	default:
		return "", false
	}
}

func (h *Handler) authorizeAPIKeyManagement(ctx context.Context, q *sqlcgen.Queries, actorID, tenantID, apiKeyID int64) error {
	keyType, err := q.GetAPIKeyType(ctx, apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errKeyNotInTenant
	}
	if err != nil {
		return fmt.Errorf("get api key type: %w", err)
	}
	object, ok := apiKeyObjectForType(tenant.KeyType(keyType))
	if !ok {
		return fmt.Errorf("%w: %q", errInvalidKeyType, keyType)
	}
	if h.rbac == nil {
		return rbac.ErrAuthorizerUnavailable
	}
	allowed, err := h.rbac.CanControlPanel(actorID, tenantID, object, rbac.ActionManage)
	if err != nil {
		return fmt.Errorf("authorize api key management: %w", err)
	}
	if !allowed {
		return errAPIKeyManageDenied
	}
	return nil
}

func (h *Handler) updateAPIKeyManagedScopes(ctx context.Context, actorID, tenantID, apiKeyID int64, desired []string) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	for _, scope := range desired {
		if !isManagedAPIKeyScope(scope) {
			return errInvalidScope
		}
	}
	ctx = db.WithTenant(ctx, tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := h.authorizeAPIKeyManagement(ctx, q, actorID, tenantID, apiKeyID); err != nil {
			return err
		}
		row, err := q.GetAPIKeyScopes(ctx, apiKeyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errKeyNotInTenant
		}
		if err != nil {
			return fmt.Errorf("get api key scopes: %w", err)
		}
		changes := managedScopeChanges(row.Scopes, desired)
		if len(changes) == 0 {
			return nil
		}
		for _, change := range changes {
			if change.Grant && !h.scopeGrantable(ctx, tenantID, row.ProjectID, change.Scope) {
				return errScopeNotGrantable
			}
		}
		next := applyManagedScopes(row.Scopes, desired)
		if err := q.SetAPIKeyScopes(ctx, sqlcgen.SetAPIKeyScopesParams{ID: apiKeyID, Scopes: next}); err != nil {
			return fmt.Errorf("set api key scopes: %w", err)
		}
		for _, change := range changes {
			action := "control_panel.api_key.scope_revoke"
			if change.Grant {
				action = "control_panel.api_key.scope_grant"
			}
			if err := auditlog.WritePlatform(ctx, tx, actorID, action, strconv.FormatInt(apiKeyID, 10), map[string]any{
				"scope":     change.Scope,
				"tenant_id": tenantID,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if h.cache != nil {
		_ = h.cache.Delete(ctx, ratelimit.APIKeyBucketKey(apiKeyID))
	}
	return nil
}

func (h *Handler) listProjects(ctx context.Context, tenantID int64) ([]ProjectOption, error) {
	if tenantID <= 0 {
		return nil, errInvalidTenant
	}
	if h.pool == nil {
		return nil, errors.New(msgControlPanelPoolNeeded)
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
		return ProjectOption{}, errors.New(msgControlPanelPoolNeeded)
	}
	var out ProjectOption
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := checkProjectQuota(ctx, q); err != nil {
			return err
		}
		row, err := q.CreateProjectForTenant(ctx, name)
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
	if err != nil {
		var qe *quota.ErrQuotaExceeded
		if errors.As(err, &qe) {
			h.metrics.QuotaRejection(qe.Axis)
		}
		return ProjectOption{}, err
	}
	return out, nil
}

// checkProjectQuota rejects a new project when the tenant enforces quotas and
// is already at its class project limit. A no-op for unenforced tenants
// (zero-config self-host stays uncapped). Runs inside the create tx.
func checkProjectQuota(ctx context.Context, q *sqlcgen.Queries) error {
	qc, err := q.GetTenantQuotaContext(ctx)
	if err != nil {
		return fmt.Errorf("tenant quota context: %w", err)
	}
	if !qc.EnforceQuotas {
		return nil
	}
	count, err := q.CountProjectsForTenant(ctx)
	if err != nil {
		return fmt.Errorf("count projects: %w", err)
	}
	limits := quota.LimitsForClass(tenant.ClampTier(int(qc.Tier)))
	return limits.CheckProjects(count)
}

func (h *Handler) createAPIKey(ctx context.Context, actorID int64, in createKeyInput) (createKeyResult, error) {
	if in.TenantID <= 0 {
		return createKeyResult{}, errInvalidTenant
	}
	if in.KeyType != tenant.KeyTypePublishable && in.KeyType != tenant.KeyTypeSecret {
		return createKeyResult{}, errInvalidKeyType
	}
	if h.pool == nil {
		return createKeyResult{}, errors.New(msgControlPanelPoolNeeded)
	}

	apiKey, err := randomAPIKey(in.KeyType)
	if err != nil {
		return createKeyResult{}, err
	}
	sum := sha256.Sum256([]byte(apiKey))

	var row sqlcgen.CreateControlPanelAPIKeyRow
	ctx = db.WithTenant(ctx, in.TenantID)
	err = h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).CreateControlPanelAPIKey(ctx, sqlcgen.CreateControlPanelAPIKeyParams{
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
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.api_key.create", strconv.FormatInt(row.ID, 10), map[string]any{
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
		q := sqlcgen.New(tx)
		if err := h.authorizeAPIKeyManagement(ctx, q, actorID, tenantID, apiKeyID); err != nil {
			return err
		}
		if err := q.UpdateAPIKeyLabel(ctx, sqlcgen.UpdateAPIKeyLabelParams{
			ID:    apiKeyID,
			Label: strings.TrimSpace(label),
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.api_key.relabel", strconv.FormatInt(apiKeyID, 10), map[string]any{"label": label, "tenant_id": tenantID})
	})
}

func (h *Handler) revokeAPIKey(ctx context.Context, actorID, tenantID, apiKeyID int64) error {
	if tenantID <= 0 {
		return errInvalidTenant
	}
	ctx = db.WithTenant(ctx, tenantID)
	if err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := h.authorizeAPIKeyManagement(ctx, q, actorID, tenantID, apiKeyID); err != nil {
			return err
		}
		if err := q.RevokeAPIKey(ctx, apiKeyID); err != nil {
			return err
		}
		if h.rbac != nil {
			if err := h.rbac.RemoveAPIKeyRolesTx(ctx, tx, apiKeyID); err != nil {
				return fmt.Errorf("rbac api key revoke: %w", err)
			}
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.api_key.revoke", strconv.FormatInt(apiKeyID, 10), map[string]any{"tenant_id": tenantID})
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
