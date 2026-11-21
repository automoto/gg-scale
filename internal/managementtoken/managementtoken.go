// Package managementtoken authenticates management tokens (ggm_...). A token
// is its own principal for one Game Project, used by the /v1/project API and
// by the /mcp endpoint. The person who created it is an upper limit on what
// it can do, checked on each request, never a source of rights.
package managementtoken

import (
	"slices"

	"github.com/automoto/gg-scale/internal/rbac"
)

// TokenPrefix starts each management token value.
const TokenPrefix = "ggm_"

// Write scopes. A token can always use the read operations.
const (
	ScopeConfigWrite       = "config:write"
	ScopeLeaderboardsWrite = "leaderboards:write"
	ScopeKeysCreate        = "keys:create"
	ScopeOriginsWrite      = "origins:write"
)

// ScopePair is a write scope and the Casbin pair that guards it.
type ScopePair struct {
	Scope  string
	Pair   func(projectID int64) string
	Action string
}

// WriteScopes lists each write scope with the Casbin pair the creator must
// pass to grant it. The dashboard offers only the scopes the person passes.
var WriteScopes = []ScopePair{
	{Scope: ScopeConfigWrite, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
	{Scope: ScopeLeaderboardsWrite, Pair: rbac.ProjectLeaderboardObject, Action: rbac.ActionManage},
	{Scope: ScopeKeysCreate, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage},
	{Scope: ScopeOriginsWrite, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
}

// WriteScope returns the entry of WriteScopes for scope.
func WriteScope(scope string) (ScopePair, bool) {
	i := slices.IndexFunc(WriteScopes, func(s ScopePair) bool { return s.Scope == scope })
	if i < 0 {
		return ScopePair{}, false
	}
	return WriteScopes[i], true
}

// Principal is the authenticated token. It holds no dashboard session.
type Principal struct {
	TokenID         int64
	TenantID        int64
	ProjectID       int64
	CreatedByUserID int64
	Label           string
	Scopes          []string
}

// HasScope reports whether the token has scope.
func (p Principal) HasScope(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}
