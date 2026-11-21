package projectadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

var (
	// ErrAPIKeyLimit means the project is at its active key limit.
	ErrAPIKeyLimit = errors.New("projectadmin: the Game Project is at its API key limit")
	// ErrScopeNotGrantable means a requested key scope is not enabled for the
	// tenant or project, or its server switch is off. Management API clients see this
	// text, so it has no package prefix.
	ErrScopeNotGrantable = errors.New("scope cannot be granted")
	// ErrProjectNotInTenant means the key's project is not in the tenant.
	ErrProjectNotInTenant = errors.New("projectadmin: Game Project is not in the Account Tenant")
	// ErrAPIKeyNotFound means the key is not in the tenant, or not in the
	// project the caller works in.
	ErrAPIKeyNotFound = errors.New("projectadmin: api key not found")
	// ErrAPIKeyManageDenied means the actor may not manage keys of this type.
	ErrAPIKeyManageDenied = errors.New("projectadmin: api key management forbidden")
	// ErrInvalidKeyScope means a feature is not one a key can carry. Management
	// API clients see this text.
	ErrInvalidKeyScope = errors.New("unknown feature scope")
)

// ManagedKeyScopes are the feature scopes that a person or a management
// token can grant or revoke on a key.
var ManagedKeyScopes = []string{
	tenant.ScopeMatchmaker,
	tenant.ScopeFleet,
	tenant.ScopeP2PRelay,
}

// IsManagedKeyScope reports whether scope is one of ManagedKeyScopes.
func IsManagedKeyScope(scope string) bool {
	return slices.Contains(ManagedKeyScopes, scope)
}

type scopeChange struct {
	Scope string
	Grant bool
}

func managedScopeChanges(current, desired []string) []scopeChange {
	changes := make([]scopeChange, 0, len(ManagedKeyScopes))
	for _, scope := range ManagedKeyScopes {
		wants := slices.Contains(desired, scope)
		if slices.Contains(current, scope) == wants {
			continue
		}
		changes = append(changes, scopeChange{Scope: scope, Grant: wants})
	}
	return changes
}

// applyManagedScopes keeps the scopes of current that are not managed and
// sets the managed ones to desired.
func applyManagedScopes(current, desired []string) []string {
	next := make([]string, 0, len(current)+len(desired))
	for _, scope := range current {
		if !IsManagedKeyScope(scope) && !slices.Contains(next, scope) {
			next = append(next, scope)
		}
	}
	for _, scope := range ManagedKeyScopes {
		if slices.Contains(desired, scope) {
			next = append(next, scope)
		}
	}
	return next
}

// APIKeyObject returns the Casbin object that guards keys of keyType.
func APIKeyObject(keyType string) (string, bool) {
	switch tenant.KeyType(keyType) {
	case tenant.KeyTypePublishable:
		return rbac.ObjectAPIKeyPublic, true
	case tenant.KeyTypeSecret:
		return rbac.ObjectAPIKeySecret, true
	default:
		return "", false
	}
}

// KeyScope limits which keys a change may touch. The zero value is the
// dashboard: any key of the tenant, revoked keys included. A management
// token sets both fields, so a request never reaches a key of another
// Game Project or a revoked key.
type KeyScope struct {
	// ProjectID, when set, limits the key to that project. A key with no
	// project does not match it.
	ProjectID *int64
	// LiveOnly refuses a revoked key.
	LiveOnly bool
}

// AuthorizeAPIKey checks that the key is in the tenant and in scope, and that the actor may manage keys of its type. A key of
// another project answers ErrAPIKeyNotFound before any permission check, so
// a caller learns nothing about it. It returns the key's scopes and project.
func AuthorizeAPIKey(ctx context.Context, q *sqlcgen.Queries, authz *rbac.Authorizer, tenantID int64, scope KeyScope, apiKeyID int64, actor Actor) (sqlcgen.GetAPIKeyScopesRow, error) {
	row, err := q.GetAPIKeyScopes(ctx, apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrAPIKeyNotFound
	}
	if err != nil {
		return row, fmt.Errorf("get api key scopes: %w", err)
	}
	if scope.ProjectID != nil && (row.ProjectID == nil || *row.ProjectID != *scope.ProjectID) {
		return row, ErrAPIKeyNotFound
	}
	if scope.LiveOnly && row.RevokedAt.Valid {
		return row, ErrAPIKeyNotFound
	}
	keyType, err := q.GetAPIKeyType(ctx, apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrAPIKeyNotFound
	}
	if err != nil {
		return row, fmt.Errorf("get api key type: %w", err)
	}
	object, ok := APIKeyObject(keyType)
	if !ok {
		return row, fmt.Errorf("unknown api key type %q", keyType)
	}
	if authz == nil {
		return row, rbac.ErrAuthorizerUnavailable
	}
	allowed, err := authz.CanControlPanel(actor.authzUserID(), tenantID, object, rbac.ActionManage)
	if err != nil {
		return row, fmt.Errorf("authorize api key management: %w", err)
	}
	if !allowed {
		return row, ErrAPIKeyManageDenied
	}
	return row, nil
}

// UpdateAPIKeyLabel renames a key. scope limits the keys it may touch.
// Authorization is as in AuthorizeAPIKey.
func UpdateAPIKeyLabel(ctx context.Context, pool *db.Pool, authz *rbac.Authorizer, tenantID int64, scope KeyScope, apiKeyID int64, label string, actor Actor) error {
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := AuthorizeAPIKey(ctx, q, authz, tenantID, scope, apiKeyID, actor)
		if err != nil {
			return err
		}
		label = strings.TrimSpace(label)
		if err := q.UpdateAPIKeyLabel(ctx, sqlcgen.UpdateAPIKeyLabelParams{ID: apiKeyID, Label: label}); err != nil {
			return err
		}
		var keyProject int64
		if row.ProjectID != nil {
			keyProject = *row.ProjectID
		}
		return actor.audit(ctx, tx, tenantID, keyProject, "control_panel.api_key.relabel", strconv.FormatInt(apiKeyID, 10), map[string]any{"label": label})
	})
}

// UpdateAPIKeyFeatures sets the managed feature scopes of a key to desired.
// A scope that is new on the key must be grantable. scope limits the keys it
// may touch.
func UpdateAPIKeyFeatures(ctx context.Context, pool *db.Pool, authz *rbac.Authorizer, sw KeySwitches, tenantID int64, scope KeyScope, apiKeyID int64, desired []string, actor Actor) error {
	if slices.ContainsFunc(desired, func(s string) bool { return !IsManagedKeyScope(s) }) {
		return ErrInvalidKeyScope
	}
	ctx = db.WithTenant(ctx, tenantID)
	return pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := AuthorizeAPIKey(ctx, q, authz, tenantID, scope, apiKeyID, actor)
		if err != nil {
			return err
		}
		changes := managedScopeChanges(row.Scopes, desired)
		for _, change := range changes {
			if change.Grant && !ScopeGrantable(ctx, authz, sw, tenantID, row.ProjectID, change.Scope) {
				return fmt.Errorf("%w: %q", ErrScopeNotGrantable, change.Scope)
			}
		}
		if len(changes) == 0 {
			return nil
		}
		if err := q.SetAPIKeyScopes(ctx, sqlcgen.SetAPIKeyScopesParams{ID: apiKeyID, Scopes: applyManagedScopes(row.Scopes, desired)}); err != nil {
			return fmt.Errorf("set api key scopes: %w", err)
		}
		var keyProject int64
		if row.ProjectID != nil {
			keyProject = *row.ProjectID
		}
		for _, change := range changes {
			action := "control_panel.api_key.scope_revoke"
			if change.Grant {
				action = "control_panel.api_key.scope_grant"
			}
			if err := actor.audit(ctx, tx, tenantID, keyProject, action, strconv.FormatInt(apiKeyID, 10), map[string]any{"scope": change.Scope}); err != nil {
				return err
			}
		}
		return nil
	})
}

// KeySwitches are the server feature switches that gate key scopes.
type KeySwitches struct {
	FleetEnabled bool
	RelayEnabled bool
}

// keyScopeFeatures maps each feature scope a key can carry to its
// feature_grant gate.
var keyScopeFeatures = map[string]rbac.Feature{
	tenant.ScopeFleet:      rbac.FeatureDedicatedServers,
	tenant.ScopeP2PRelay:   rbac.FeatureP2PRelay,
	tenant.ScopeMatchmaker: rbac.FeatureMatchmaker,
}

// ScopeGrantable reports whether a key scope can be granted: the server
// switch must be on (matchmaker has none) and a feature grant must enable the
// feature for the key's tenant and project. A key with no project uses the
// tenant-level grant.
func ScopeGrantable(ctx context.Context, authz *rbac.Authorizer, sw KeySwitches, tenantID int64, projectID *int64, scope string) bool {
	feature, ok := keyScopeFeatures[scope]
	if !ok || authz == nil {
		return false
	}
	if (scope == tenant.ScopeFleet && !sw.FleetEnabled) || (scope == tenant.ScopeP2PRelay && !sw.RelayEnabled) {
		return false
	}
	var pid int64
	if projectID != nil {
		pid = *projectID
	}
	enabled, err := authz.FeatureEnabled(ctx, tenantID, pid, feature)
	return err == nil && enabled
}

// NewAPIKey is the input of CreateAPIKey.
type NewAPIKey struct {
	ProjectID *int64
	Label     string
	Type      tenant.KeyType
	// Scopes are extra feature scopes. The matchmaker scope is always added.
	Scopes []string
	// MaxActive, when > 0, refuses the create when the project already has
	// this many active keys. It needs a ProjectID.
	MaxActive int64
	Switches  KeySwitches
}

// CreateAPIKey makes a key, its Casbin role, its scopes, and its audit row
// in one transaction, then reloads the policy. It does no authorization: the
// caller checks that the actor may manage this key type. The key value is
// returned once and is never stored.
func CreateAPIKey(ctx context.Context, pool *db.Pool, authz *rbac.Authorizer, tenantID int64, in NewAPIKey, actor Actor) (int64, string, error) {
	scopes := []string{tenant.ScopeMatchmaker}
	for _, s := range in.Scopes {
		if slices.Contains(scopes, s) {
			continue
		}
		if !ScopeGrantable(ctx, authz, in.Switches, tenantID, in.ProjectID, s) {
			return 0, "", fmt.Errorf("%w: %q", ErrScopeNotGrantable, s)
		}
		scopes = append(scopes, s)
	}
	value, err := NewAPIKeyValue(in.Type)
	if err != nil {
		return 0, "", err
	}
	sum := sha256.Sum256([]byte(value))
	var id int64
	ctx = db.WithTenant(ctx, tenantID)
	err = pool.Q(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if in.MaxActive > 0 && in.ProjectID != nil {
			if _, err := q.LockLiveProject(ctx, *in.ProjectID); errors.Is(err, pgx.ErrNoRows) {
				return ErrProjectNotInTenant
			} else if err != nil {
				return err
			}
			n, err := q.CountActiveAPIKeysForProject(ctx, in.ProjectID)
			if err != nil {
				return err
			}
			if n >= in.MaxActive {
				return ErrAPIKeyLimit
			}
		}
		row, err := q.CreateControlPanelAPIKey(ctx, sqlcgen.CreateControlPanelAPIKeyParams{
			ProjectID: in.ProjectID,
			KeyHash:   sum[:],
			Label:     strings.TrimSpace(in.Label),
			KeyType:   string(in.Type),
			Scopes:    scopes,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotInTenant
		}
		if err != nil {
			return fmt.Errorf("create api key: %w", err)
		}
		id = row.ID
		if authz != nil {
			if err := authz.AddAPIKeyRoleTx(ctx, tx, id, tenantID, in.Type); err != nil {
				return fmt.Errorf("rbac api key create: %w", err)
			}
		}
		var projectID int64
		if in.ProjectID != nil {
			projectID = *in.ProjectID
		}
		return actor.audit(ctx, tx, tenantID, projectID, "control_panel.api_key.create", strconv.FormatInt(id, 10), map[string]any{
			"label":    in.Label,
			"key_type": string(in.Type),
			"scopes":   scopes,
		})
	})
	if err != nil {
		return 0, "", err
	}
	if authz != nil {
		if err := authz.ReloadPolicy(); err != nil {
			slog.WarnContext(ctx, "rbac reload after api key create", "err", err)
		}
	}
	return id, value, nil
}

// NewAPIKeyValue mints a plaintext API key with a type prefix: ggp_ for
// publishable, ggs_ for secret, then 32 random bytes in base64url. The prefix
// is part of the stored hash, so it helps log searches and leak detection
// without changing server policy.
func NewAPIKeyValue(keyType tenant.KeyType) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("api key rand: %w", err)
	}
	prefix := "ggs_"
	if keyType == tenant.KeyTypePublishable {
		prefix = "ggp_"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
