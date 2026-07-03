package rbac

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	stringadapter "github.com/casbin/casbin/v3/persist/string-adapter"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/tenant"
)

//go:embed model.conf
var modelFS embed.FS

const defaultPolicyReloadInterval = 10 * time.Second
const featureGrantCacheTTL = 5 * time.Second

// ErrAuthorizerUnavailable is returned when a mutating RBAC operation is called
// without a configured authorizer.
var ErrAuthorizerUnavailable = errors.New("rbac: authorizer unavailable")

// Actions are the stable Casbin action names used by routes and policies.
const (
	ActionRead       = "read"
	ActionCreate     = "create"
	ActionUpdate     = "update"
	ActionDelete     = "delete"
	ActionManage     = "manage"
	ActionInvite     = "invite"
	ActionRevoke     = "revoke"
	ActionDisable    = "disable"
	ActionAllocate   = "allocate"
	ActionDeallocate = "deallocate"
	// #nosec G101 -- This is a Casbin action name, not a credential value.
	ActionIssueCredentials = "issue_credentials"
	ActionCreateTicket     = "create_ticket"
	ActionSubmit           = "submit"
	ActionVerify           = "verify"
	ActionConnect          = "connect"
)

// Objects are the stable Casbin object names used by routes and policies.
const (
	ObjectTenant          = "tenant"
	ObjectProject         = "project"
	ObjectAPIKey          = "api_key"
	ObjectAPIKeySecret    = "api_key:secret"
	ObjectAPIKeyPublic    = "api_key:publishable"
	ObjectTeam            = "team"
	ObjectAudit           = "audit"
	ObjectAuth            = "auth"
	ObjectProfile         = "profile"
	ObjectStorage         = "storage"
	ObjectFriends         = "friends"
	ObjectLeaderboard     = "leaderboard"
	ObjectRealtime        = "realtime"
	ObjectPlayer          = "player"
	ObjectCustomToken     = "custom_token"
	ObjectFeatureRequest  = "feature_request"
	ObjectDashboardUser   = "dashboard_user"
	ObjectPlatformPlugins = "platform:plugins"
)

// Roles are the stable Casbin role names used in grouping policy.
const (
	RolePlatformOwner   = "role:platform_owner"
	RolePlatformAdmin   = "role:platform_admin"
	RolePlatformSupport = "role:platform_support"

	RoleTenantOwner   = "role:tenant_owner"
	RoleTenantAdmin   = "role:tenant_admin"
	RoleSecurityAdmin = "role:security_admin"
	RoleDeveloper     = "role:developer"
	RoleSupport       = "role:support"
	RoleAnalyst       = "role:analyst"
	RoleFleetOperator = "role:fleet_operator"

	RolePlayerStandard = "role:player_standard"
	RolePlayerVerified = "role:player_verified"
	RolePlayerBanned   = "role:player_banned"

	RoleAPIClient       = "role:api_client"
	RoleAPIServer       = "role:api_server"
	RoleAPIFleetRuntime = "role:api_fleet_runtime"
)

// Feature is the name of a high-risk tenant or project feature gate.
type Feature string

// Features are database-backed gates layered on top of Casbin permissions.
const (
	FeatureP2PRelay           Feature = "p2p_relay"
	FeatureDedicatedServers   Feature = "dedicated_servers"
	FeatureFleetDockerBackend Feature = "fleet_docker_backend"
	FeatureFleetAgonesBackend Feature = "fleet_agones_backend"
	FeatureFleetPluginBackend Feature = "fleet_plugin_backend"
)

// DashboardUser is the authorization-relevant view of a dashboard user.
type DashboardUser struct {
	ID              int64
	IsPlatformAdmin bool
}

// Authorizer wraps the Casbin enforcer and feature-grant checks.
type Authorizer struct {
	enforcer     *casbin.SyncedEnforcer
	pool         *db.Pool
	featureCache map[featureCacheKey]featureCacheEntry
	featureMu    sync.Mutex
}

type featureCacheKey struct {
	tenantID  int64
	projectID int64
	feature   Feature
}

type featureCacheEntry struct {
	enabled   bool
	expiresAt time.Time
}

// NewAuthorizer loads persisted Casbin policy from Postgres.
func NewAuthorizer(pool *db.Pool) (*Authorizer, error) {
	if pool == nil {
		return nil, fmt.Errorf("rbac: pool is required")
	}
	e, err := newEnforcer(newAdapter(pool), true)
	if err != nil {
		return nil, err
	}
	e.StartAutoLoadPolicy(defaultPolicyReloadInterval)
	return &Authorizer{enforcer: e, pool: pool, featureCache: make(map[featureCacheKey]featureCacheEntry)}, nil
}

// NewMemoryAuthorizer builds an authorizer with the default policy in memory.
func NewMemoryAuthorizer() (*Authorizer, error) {
	e, err := newEnforcer(stringadapter.NewAdapter(defaultPolicyCSV), false)
	if err != nil {
		return nil, err
	}
	return &Authorizer{enforcer: e, featureCache: make(map[featureCacheKey]featureCacheEntry)}, nil
}

// Close stops background policy reload.
func (a *Authorizer) Close() {
	if a == nil {
		return
	}
	a.enforcer.StopAutoLoadPolicy()
}

// ReloadPolicy refreshes the in-memory enforcer from persistent policy.
func (a *Authorizer) ReloadPolicy() error {
	if a == nil {
		return nil
	}
	if err := a.enforcer.LoadPolicy(); err != nil {
		return fmt.Errorf("rbac: reload policy: %w", err)
	}
	return nil
}

func newEnforcer(adapter any, autoSave bool) (*casbin.SyncedEnforcer, error) {
	text, err := modelFS.ReadFile("model.conf")
	if err != nil {
		return nil, fmt.Errorf("rbac: read model: %w", err)
	}
	m, err := model.NewModelFromString(string(text))
	if err != nil {
		return nil, fmt.Errorf("rbac: parse model: %w", err)
	}
	e, err := casbin.NewSyncedEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("rbac: new enforcer: %w", err)
	}
	e.EnableAutoSave(autoSave)
	return e, nil
}

// CanDashboard reports whether a dashboard user can perform act on obj.
func (a *Authorizer) CanDashboard(user DashboardUser, tenantID int64, obj, act string) (bool, error) {
	if a == nil {
		return false, nil
	}
	dom := TenantDomain(tenantID)
	allowed, err := a.enforce(DashboardSubject(user.ID), dom, obj, act)
	if err != nil || allowed {
		return allowed, err
	}
	if user.IsPlatformAdmin {
		return a.enforce(RolePlatformAdmin, dom, obj, act)
	}
	return false, nil
}

// CanAPIKey reports whether an API key can perform act on obj.
func (a *Authorizer) CanAPIKey(key tenant.APIKey, obj, act string) (bool, error) {
	if a == nil {
		return false, nil
	}
	dom := TenantDomain(key.TenantID)
	allowed, err := a.enforce(APIKeySubject(key.ID), dom, obj, act)
	if err != nil || allowed {
		return allowed, err
	}
	role, ok := APIKeyRole(key.Type)
	if !ok {
		return false, nil
	}
	return a.enforce(role, dom, obj, act)
}

// CanPlayer reports whether a player can perform act on obj.
func (a *Authorizer) CanPlayer(tenantID, playerID int64, obj, act string) (bool, error) {
	if a == nil {
		return false, nil
	}
	dom := TenantDomain(tenantID)
	allowed, err := a.enforce(PlayerSubject(playerID), dom, obj, act)
	if err != nil || allowed {
		return allowed, err
	}
	return a.enforce(RolePlayerStandard, dom, obj, act)
}

// FeatureEnabled reports whether a high-risk feature is enabled.
func (a *Authorizer) FeatureEnabled(ctx context.Context, tenantID, projectID int64, feature Feature) (bool, error) {
	if a == nil || a.pool == nil {
		return false, nil
	}
	key := featureCacheKey{tenantID: tenantID, projectID: projectID, feature: feature}
	now := time.Now()
	a.featureMu.Lock()
	if entry, ok := a.featureCache[key]; ok && now.Before(entry.expiresAt) {
		a.featureMu.Unlock()
		return entry.enabled, nil
	}
	a.featureMu.Unlock()

	var enabled bool
	err := a.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", strconv.FormatInt(tenantID, 10)); err != nil {
			return err
		}
		const query = `
SELECT enabled
FROM feature_grants
WHERE tenant_id = $1
  AND feature = $2
  AND (
    project_id IS NULL
    OR ($3::bigint > 0 AND project_id = $3)
  )
ORDER BY (project_id IS NULL), updated_at DESC, id DESC
LIMIT 1`
		return tx.QueryRow(ctx, query, tenantID, string(feature), projectID).Scan(&enabled)
	})
	if err == nil {
		a.storeFeatureCache(key, enabled, now)
		return enabled, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		a.storeFeatureCache(key, false, now)
		return false, nil
	}
	return false, err
}

func (a *Authorizer) storeFeatureCache(key featureCacheKey, enabled bool, now time.Time) {
	a.featureMu.Lock()
	defer a.featureMu.Unlock()
	a.featureCache[key] = featureCacheEntry{
		enabled:   enabled,
		expiresAt: now.Add(featureGrantCacheTTL),
	}
}

// SetDashboardMembershipRole replaces a dashboard user's tenant role.
func (a *Authorizer) SetDashboardMembershipRole(userID, tenantID int64, membershipRole string) error {
	role, ok := DashboardMembershipRole(membershipRole)
	if !ok {
		return fmt.Errorf("rbac: unknown dashboard membership role %q", membershipRole)
	}
	return a.setSubjectRole(DashboardSubject(userID), role, TenantDomain(tenantID))
}

// SetDashboardMembershipRoleTx writes a dashboard user's tenant role in tx.
func (a *Authorizer) SetDashboardMembershipRoleTx(ctx context.Context, tx pgx.Tx, userID, tenantID int64, membershipRole string) error {
	role, ok := DashboardMembershipRole(membershipRole)
	if !ok {
		return fmt.Errorf("rbac: unknown dashboard membership role %q", membershipRole)
	}
	return a.setSubjectRoleTx(ctx, tx, DashboardSubject(userID), role, TenantDomain(tenantID))
}

// GrantableDashboardRole reports whether role is an à-la-carte dashboard role a
// tenant admin may grant on top of a user's membership role. These coexist
// with the membership role rather than replacing it.
func GrantableDashboardRole(role string) bool {
	switch role {
	case RoleFleetOperator:
		return true
	default:
		return false
	}
}

// AddDashboardRole grants an à-la-carte dashboard role to a user, alongside
// their membership role.
func (a *Authorizer) AddDashboardRole(userID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !GrantableDashboardRole(role) {
		return fmt.Errorf("rbac: role %q is not grantable", role)
	}
	_, err := a.enforcer.AddGroupingPolicy(DashboardSubject(userID), role, TenantDomain(tenantID))
	return err
}

// RemoveDashboardRole revokes a single à-la-carte dashboard role from a user,
// leaving their membership role intact.
func (a *Authorizer) RemoveDashboardRole(userID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !GrantableDashboardRole(role) {
		return fmt.Errorf("rbac: role %q is not grantable", role)
	}
	_, err := a.enforcer.RemoveFilteredNamedGroupingPolicy("g", 0, DashboardSubject(userID), role, TenantDomain(tenantID))
	return err
}

// AddDashboardRoleTx grants an à-la-carte dashboard role to a user in tx,
// alongside their membership role.
func (a *Authorizer) AddDashboardRoleTx(ctx context.Context, tx pgx.Tx, userID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !GrantableDashboardRole(role) {
		return fmt.Errorf("rbac: role %q is not grantable", role)
	}
	return insertRule(ctx, tx, "g", []string{DashboardSubject(userID), role, TenantDomain(tenantID)})
}

// RemoveDashboardRoleTx revokes a single à-la-carte dashboard role from a user
// in tx, leaving their membership role intact.
func (a *Authorizer) RemoveDashboardRoleTx(ctx context.Context, tx pgx.Tx, userID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !GrantableDashboardRole(role) {
		return fmt.Errorf("rbac: role %q is not grantable", role)
	}
	return removeFilteredRule(ctx, tx, "g", 0, DashboardSubject(userID), role, TenantDomain(tenantID))
}

// HasDashboardRole reports whether a user holds an explicit role grant in a
// tenant. Reads the in-memory enforcer, so callers that just mutated policy
// should ReloadPolicy first.
func (a *Authorizer) HasDashboardRole(userID, tenantID int64, role string) (bool, error) {
	if a == nil {
		return false, nil
	}
	rules, err := a.enforcer.GetFilteredNamedGroupingPolicy("g", 0, DashboardSubject(userID), role, TenantDomain(tenantID))
	if err != nil {
		return false, fmt.Errorf("rbac: get grouping policy: %w", err)
	}
	return len(rules) > 0, nil
}

// AddPlatformAdmin grants the global platform-admin role to a dashboard user.
func (a *Authorizer) AddPlatformAdmin(userID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	_, err := a.enforcer.AddGroupingPolicy(DashboardSubject(userID), RolePlatformAdmin, "*")
	return err
}

// AddPlatformAdminTx writes the global platform-admin role in tx.
func (a *Authorizer) AddPlatformAdminTx(ctx context.Context, tx pgx.Tx, userID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	return insertRule(ctx, tx, "g", []string{DashboardSubject(userID), RolePlatformAdmin, "*"})
}

// AddAPIKeyRole replaces an API key's tenant role based on its key type.
func (a *Authorizer) AddAPIKeyRole(keyID, tenantID int64, keyType tenant.KeyType) error {
	role, ok := APIKeyRole(keyType)
	if !ok {
		return fmt.Errorf("rbac: unknown api key type %q", keyType)
	}
	return a.setSubjectRole(APIKeySubject(keyID), role, TenantDomain(tenantID))
}

// AddAPIKeyRoleTx writes an API key's tenant role in tx.
func (a *Authorizer) AddAPIKeyRoleTx(ctx context.Context, tx pgx.Tx, keyID, tenantID int64, keyType tenant.KeyType) error {
	role, ok := APIKeyRole(keyType)
	if !ok {
		return fmt.Errorf("rbac: unknown api key type %q", keyType)
	}
	return a.setSubjectRoleTx(ctx, tx, APIKeySubject(keyID), role, TenantDomain(tenantID))
}

// AddPlayerRole grants an explicit tenant role to a player.
func (a *Authorizer) AddPlayerRole(playerID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !PlayerRole(role) {
		return fmt.Errorf("rbac: unknown player role %q", role)
	}
	_, err := a.enforcer.AddGroupingPolicy(PlayerSubject(playerID), role, TenantDomain(tenantID))
	return err
}

// AddPlayerRoleTx writes an explicit tenant role for a player in tx.
func (a *Authorizer) AddPlayerRoleTx(ctx context.Context, tx pgx.Tx, playerID, tenantID int64, role string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if !PlayerRole(role) {
		return fmt.Errorf("rbac: unknown player role %q", role)
	}
	return insertRule(ctx, tx, "g", []string{PlayerSubject(playerID), role, TenantDomain(tenantID)})
}

// RemoveDashboardRoles removes a dashboard user's tenant-scoped roles.
func (a *Authorizer) RemoveDashboardRoles(userID, tenantID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	_, err := a.enforcer.RemoveFilteredNamedGroupingPolicy("g", 0, DashboardSubject(userID), "", TenantDomain(tenantID))
	return err
}

// RemoveDashboardRolesTx removes a dashboard user's tenant-scoped roles in tx.
func (a *Authorizer) RemoveDashboardRolesTx(ctx context.Context, tx pgx.Tx, userID, tenantID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	return removeFilteredRule(ctx, tx, "g", 0, DashboardSubject(userID), "", TenantDomain(tenantID))
}

// RemoveAPIKeyRoles removes all roles for an API key.
func (a *Authorizer) RemoveAPIKeyRoles(keyID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	_, err := a.enforcer.RemoveFilteredNamedGroupingPolicy("g", 0, APIKeySubject(keyID))
	return err
}

// RemoveAPIKeyRolesTx removes all roles for an API key in tx.
func (a *Authorizer) RemoveAPIKeyRolesTx(ctx context.Context, tx pgx.Tx, keyID int64) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	return removeFilteredRule(ctx, tx, "g", 0, APIKeySubject(keyID))
}

func (a *Authorizer) setSubjectRole(subject, role, domain string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	existing, err := a.enforcer.GetFilteredNamedGroupingPolicy("g", 0, subject, "", domain)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		_, err := a.enforcer.AddGroupingPolicy(subject, role, domain)
		return err
	}
	_, err = a.enforcer.UpdateGroupingPolicy(existing[0], []string{subject, role, domain})
	if err != nil {
		return err
	}
	for _, duplicate := range existing[1:] {
		if _, err := a.enforcer.RemoveGroupingPolicy(ruleArgs(duplicate)...); err != nil {
			return err
		}
	}
	return nil
}

func ruleArgs(rule []string) []any {
	args := make([]any, len(rule))
	for i, v := range rule {
		args[i] = v
	}
	return args
}

func (a *Authorizer) setSubjectRoleTx(ctx context.Context, tx pgx.Tx, subject, role, domain string) error {
	if a == nil {
		return ErrAuthorizerUnavailable
	}
	if err := removeFilteredRule(ctx, tx, "g", 0, subject, "", domain); err != nil {
		return err
	}
	return insertRule(ctx, tx, "g", []string{subject, role, domain})
}

func (a *Authorizer) enforce(sub, dom, obj, act string) (bool, error) {
	allowed, err := a.enforcer.Enforce(sub, dom, obj, act)
	if err != nil {
		return false, fmt.Errorf("rbac: enforce %s %s %s %s: %w", sub, dom, obj, act, err)
	}
	return allowed, nil
}

// DashboardMembershipRole maps legacy dashboard membership roles to Casbin roles.
func DashboardMembershipRole(role string) (string, bool) {
	switch role {
	case "owner":
		return RoleTenantOwner, true
	case "admin":
		return RoleTenantAdmin, true
	case "member":
		return RoleAnalyst, true
	default:
		return "", false
	}
}

// APIKeyRole maps API key types to Casbin roles.
func APIKeyRole(keyType tenant.KeyType) (string, bool) {
	switch keyType {
	case tenant.KeyTypePublishable:
		return RoleAPIClient, true
	case tenant.KeyTypeSecret:
		return RoleAPIServer, true
	default:
		return "", false
	}
}

// PlayerRole reports whether role is safe to grant to a player subject.
func PlayerRole(role string) bool {
	switch role {
	case RolePlayerStandard, RolePlayerVerified, RolePlayerBanned:
		return true
	default:
		return false
	}
}

// BackendFeature maps fleet backend names to feature gates.
func BackendFeature(backend string) (Feature, bool) {
	switch {
	case backend == "docker":
		return FeatureFleetDockerBackend, true
	case backend == "agones":
		return FeatureFleetAgonesBackend, true
	case strings.HasPrefix(backend, "plugin"):
		return FeatureFleetPluginBackend, true
	default:
		return "", false
	}
}

// TenantDomain returns the Casbin domain for a tenant id.
func TenantDomain(tenantID int64) string {
	return "tenant:" + strconv.FormatInt(tenantID, 10)
}

// DashboardSubject returns the Casbin subject for a dashboard user.
func DashboardSubject(userID int64) string {
	return "dashboard:user:" + strconv.FormatInt(userID, 10)
}

// APIKeySubject returns the Casbin subject for an API key.
func APIKeySubject(keyID int64) string {
	return "api_key:" + strconv.FormatInt(keyID, 10)
}

// PlayerSubject returns the Casbin subject for a player.
func PlayerSubject(playerID int64) string {
	return "player:" + strconv.FormatInt(playerID, 10)
}

// ProjectPlayersObject returns the project players object name.
func ProjectPlayersObject(projectID int64) string {
	return projectObject(projectID, "players")
}

// ProjectConfigObject returns the project config object name.
func ProjectConfigObject(projectID int64) string {
	return projectObject(projectID, "config")
}

// ProjectFleetObject returns the project fleet object name.
func ProjectFleetObject(projectID int64) string {
	return projectObject(projectID, "fleet")
}

// ProjectAllocationObject returns the project allocation object name.
func ProjectAllocationObject(projectID int64) string {
	return projectObject(projectID, "allocation")
}

// ProjectMatchmakerObject returns the project matchmaker object name.
func ProjectMatchmakerObject(projectID int64) string {
	return projectObject(projectID, "matchmaker")
}

// ProjectRelayObject returns the project relay object name.
func ProjectRelayObject(projectID int64) string {
	return projectObject(projectID, "relay")
}

// ProjectDedicatedMatchmakingObject returns the project dedicated-matchmaking object name.
func ProjectDedicatedMatchmakingObject(projectID int64) string {
	return projectObject(projectID, "matchmaking:dedicated")
}

func projectObject(projectID int64, suffix string) string {
	return "project:" + strconv.FormatInt(projectID, 10) + ":" + suffix
}
