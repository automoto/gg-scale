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
	// tenant or project, or its server switch is off. MCP clients see this
	// text, so it has no package prefix.
	ErrScopeNotGrantable = errors.New("scope cannot be granted")
	// ErrProjectNotInTenant means the key's project is not in the tenant.
	ErrProjectNotInTenant = errors.New("projectadmin: Game Project is not in the Account Tenant")
)

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
