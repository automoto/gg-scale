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
	ScopeKeysWrite         = "keys:write"
	ScopeKeysRevoke        = "keys:revoke"
	ScopeKeysSecret        = "keys:secret"
	ScopeSteamWrite        = "steam:write"
	ScopeMatchmakingRead   = "matchmaking:read"
	ScopePlayersRead       = "players:read"
	ScopePlayersPII        = "players:pii"
	ScopePlayersWrite      = "players:write"
	ScopePlayersInvite     = "players:invite"
	ScopeLimitsRead        = "limits:read"
	ScopeLimitsWrite       = "limits:write"
	ScopeFleetRead         = "fleet:read"
	ScopeFleetWrite        = "fleet:write"
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

// ScopePair is a scope, its class, the Casbin pair that guards it, and the
// token page row it belongs to.
type ScopePair struct {
	Scope    string
	Class    int
	Pair     func(projectID int64) string
	Action   string
	Resource string
	// Label is the sentence of the scope's box on the token page. Only a
	// risky scope has one.
	Label string
	// Needs is a scope that the token must also have. The token page and
	// the server refuse the scope without it.
	Needs string
}

func projectObject(int64) string { return rbac.ObjectProject }

// Scopes lists each scope with the Casbin pair the creator must pass to
// grant it. The dashboard offers only the scopes the person passes. No role
// has a read pair on the project config or leaderboard objects, so their
// read scopes use (project, manage), the check of the dashboard pages. The
// players pages are open to (project, manage), and the roles that have it
// (tenant_admin and up) all have (players, manage) but only some have
// (players, read), so the player scopes use (players, manage): a pair that
// is not weaker than the pages and that each such role passes. The fleet
// pages check (fleet, read), so fleet:read uses it. The allocation reads
// also check their own pair, (allocation, read), on each request. The key
// write scopes use the publishable key pair; each request also checks the
// pair of the target key's type, so a secret key needs the secret key pair.
var Scopes = []ScopePair{
	{Scope: ScopeConfigRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage, Resource: "config"},
	{Scope: ScopeConfigWrite, Class: ClassNormal, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate, Resource: "config"},
	{Scope: ScopeOriginsWrite, Class: ClassNormal, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate, Resource: "config"},
	{Scope: ScopeLeaderboardsRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage, Resource: "leaderboards"},
	{Scope: ScopeLeaderboardsWrite, Class: ClassNormal, Pair: rbac.ProjectLeaderboardObject, Action: rbac.ActionManage, Resource: "leaderboards"},
	{Scope: ScopeKeysRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage, Resource: "keys"},
	{Scope: ScopeKeysCreate, Class: ClassNormal, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage, Resource: "keys"},
	{Scope: ScopeKeysWrite, Class: ClassNormal, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage, Resource: "keys"},
	{
		Scope: ScopeKeysRevoke, Class: ClassRisky, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage, Resource: "keys",
		Label: "Revoke API keys. Cannot be undone; a game that uses the key stops.", Needs: ScopeKeysRead,
	},
	{
		Scope: ScopeKeysSecret, Class: ClassRisky, Pair: func(int64) string { return rbac.ObjectAPIKeySecret }, Action: rbac.ActionManage, Resource: "keys",
		Label: "Create secret API keys. The key value is returned to the token user.",
	},
	{
		Scope: ScopeSteamWrite, Class: ClassRisky, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate, Resource: "config",
		Label: "Change the Steam Web API key. The old key is lost.",
	},
	{Scope: ScopeMatchmakingRead, Class: ClassNormal, Pair: rbac.ProjectMatchmakerObject, Action: rbac.ActionRead, Resource: "matchmaking"},
	{Scope: ScopePlayersRead, Class: ClassNormal, Pair: rbac.ProjectPlayersObject, Action: rbac.ActionManage, Resource: "players"},
	{
		Scope: ScopePlayersPII, Class: ClassRisky, Pair: rbac.ProjectPlayersObject, Action: rbac.ActionManage, Resource: "players",
		Label: "See player email and display name. They go into agent logs.", Needs: ScopePlayersRead,
	},
	{Scope: ScopePlayersWrite, Class: ClassNormal, Pair: rbac.ProjectPlayersObject, Action: rbac.ActionManage, Resource: "players"},
	{
		Scope: ScopePlayersInvite, Class: ClassRisky, Pair: rbac.ProjectPlayersObject, Action: rbac.ActionManage, Resource: "players",
		Label: "Invite and link players. Sends email.", Needs: ScopePlayersRead,
	},
	{Scope: ScopeLimitsRead, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage, Resource: "limits"},
	{Scope: ScopeLimitsWrite, Class: ClassNormal, Pair: projectObject, Action: rbac.ActionManage, Resource: "limits"},
	{Scope: ScopeFleetRead, Class: ClassNormal, Pair: rbac.ProjectFleetObject, Action: rbac.ActionRead, Resource: "fleets"},
	{Scope: ScopeFleetWrite, Class: ClassNormal, Pair: rbac.ProjectFleetObject, Action: rbac.ActionManage, Resource: "fleets"},
}

// Resource is one row of the access table on the token page.
type Resource struct {
	// Name is the row key: the form field is access_<Name>.
	Name  string
	Label string
	// Noun names the row in the text "Needs read access to <Noun>". The
	// Label is used when it is empty.
	Noun string
	// Read is the scope that "Read" gives.
	Read string
	// Feature, when set, is the feature grant the row needs. Its server
	// switch is needed too.
	Feature rbac.Feature
}

// Resources lists the token page rows in page order.
var Resources = []Resource{
	{Name: "config", Label: "Remote config and origins", Read: ScopeConfigRead},
	{Name: "leaderboards", Label: "Leaderboards", Read: ScopeLeaderboardsRead},
	{Name: "keys", Label: "API keys", Read: ScopeKeysRead},
	{Name: "matchmaking", Label: "Matchmaking", Read: ScopeMatchmakingRead},
	{Name: "players", Label: "Players (IDs and status)", Noun: "Players", Read: ScopePlayersRead},
	{Name: "limits", Label: "Project limits", Read: ScopeLimitsRead},
	{Name: "fleets", Label: "Fleets", Read: ScopeFleetRead, Feature: rbac.FeatureDedicatedServers},
}

// WriteScopes returns the normal-class scopes of the row other than its read
// scope: the scopes that "Read and write" adds.
func (r Resource) WriteScopes() []string {
	var out []string
	for _, s := range Scopes {
		if s.Resource == r.Name && s.Class == ClassNormal && s.Scope != r.Read {
			out = append(out, s.Scope)
		}
	}
	return out
}

// ReadScopes returns the read scope of each row.
func ReadScopes() []string {
	out := make([]string, 0, len(Resources))
	for _, r := range Resources {
		out = append(out, r.Read)
	}
	return out
}

// Scope returns the entry of Scopes for scope.
func Scope(scope string) (ScopePair, bool) {
	i := slices.IndexFunc(Scopes, func(s ScopePair) bool { return s.Scope == scope })
	if i < 0 {
		return ScopePair{}, false
	}
	return Scopes[i], true
}

// ScopeNames returns the name of each scope in Scopes.
func ScopeNames() []string {
	out := make([]string, 0, len(Scopes))
	for _, s := range Scopes {
		out = append(out, s.Scope)
	}
	return out
}

// AllowedScopes returns the scopes whose pair the creator passes now. It
// skips a scope that is not in Scopes. Every caller that asks what a creator
// may still use goes through it, so they use the same check.
func AllowedScopes(authz *rbac.Authorizer, creatorID, tenantID, projectID int64, scopes []string) ([]string, error) {
	out := []string{}
	for _, name := range scopes {
		sp, ok := Scope(name)
		if !ok {
			continue
		}
		allowed, err := authz.CanControlPanel(creatorID, tenantID, sp.Pair(projectID), sp.Action)
		if err != nil {
			return nil, err
		}
		if allowed {
			out = append(out, name)
		}
	}
	return out, nil
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
