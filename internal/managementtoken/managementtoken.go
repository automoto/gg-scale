// Package managementtoken authenticates management tokens (ggm_...). A token
// is its own principal for one Game Project, used by the /v1/project API and
// by the /mcp endpoint. The person who created it is an upper limit on what
// it can do, checked on each request, never a source of rights.
package managementtoken

import (
	"slices"
	"time"

	"github.com/automoto/gg-scale/internal/rbac"
)

// TokenPrefix starts each management token value.
const TokenPrefix = "ggm_"

// Scopes. A scope is <resource>:<action>.
const (
	ScopeConfigRead        = "config:read"
	ScopeConfigWrite       = "config:write"
	ScopeOriginsWrite      = "origins:write"
	ScopeLeaderboardsRead  = "leaderboards:read"
	ScopeLeaderboardsWrite = "leaderboards:write"
	ScopeKeysRead          = "keys:read"
	ScopeKeysCreate        = "keys:create"
	ScopeMatchmakingRead   = "matchmaking:read"
)

// Scope classes.
const (
	// ClassNormal is a scope for reversible, project-level operations that
	// return no secret. It can be in a preset.
	ClassNormal = 1
	// ClassRisky is an opt-in scope: its operations cannot be undone, return
	// or write a secret, or send email. It is never in a preset.
	ClassRisky = 2
)

// ScopePair is a scope, its class, and the Casbin pair that guards it.
type ScopePair struct {
	Scope  string
	Class  int
	Pair   func(projectID int64) string
	Action string
}

func projectObject(int64) string { return rbac.ObjectProject }

// Scopes lists each scope with the Casbin pair the creator must pass to
// grant it. The dashboard offers only the scopes the person passes. No role
// has a read pair on the project config or leaderboard objects, so their
// read scopes use (project, manage), the check of the dashboard pages.
var Scopes = []ScopePair{
	{Scope: ScopeConfigRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage},
	{Scope: ScopeConfigWrite, Class: ClassNormal, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
	{Scope: ScopeOriginsWrite, Class: ClassNormal, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
	{Scope: ScopeLeaderboardsRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage},
	{Scope: ScopeLeaderboardsWrite, Class: ClassNormal, Pair: rbac.ProjectLeaderboardObject, Action: rbac.ActionManage},
	{Scope: ScopeKeysRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage},
	{Scope: ScopeKeysCreate, Class: ClassNormal, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage},
	{Scope: ScopeMatchmakingRead, Class: ClassNormal, Pair: rbac.ProjectMatchmakerObject, Action: rbac.ActionRead},
}

// Scope returns the entry of Scopes for scope.
func Scope(scope string) (ScopePair, bool) {
	i := slices.IndexFunc(Scopes, func(s ScopePair) bool { return s.Scope == scope })
	if i < 0 {
		return ScopePair{}, false
	}
	return Scopes[i], true
}

// Principal is the authenticated token. It holds no dashboard session.
type Principal struct {
	TokenID         int64
	TenantID        int64
	ProjectID       int64
	CreatedByUserID int64
	Label           string
	Scopes          []string
	ExpiresAt       time.Time
}

// HasScope reports whether the token has scope.
func (p Principal) HasScope(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}
