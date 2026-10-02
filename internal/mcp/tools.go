package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/automoto/gg-scale/internal/rbac"
)

// Write scopes. A token can always use the read tools.
const (
	ScopeConfigWrite       = "config:write"
	ScopeLeaderboardsWrite = "leaderboards:write"
	ScopeKeysCreate        = "keys:create"
	ScopeOriginsWrite      = "origins:write"
)

// WriteScopes lists each write scope with the Casbin pair the creator must
// pass to grant it. The dashboard offers only the scopes the person passes.
var WriteScopes = []ScopePair{
	{Scope: ScopeConfigWrite, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
	{Scope: ScopeLeaderboardsWrite, Pair: rbac.ProjectLeaderboardObject, Action: rbac.ActionManage},
	{Scope: ScopeKeysCreate, Pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, Action: rbac.ActionManage},
	{Scope: ScopeOriginsWrite, Pair: rbac.ProjectConfigObject, Action: rbac.ActionUpdate},
}

// ScopePair is a write scope and the Casbin pair that guards it.
type ScopePair struct {
	Scope  string
	Pair   func(projectID int64) string
	Action string
}

// tool is one row of the tool permission table. A tool with no row does not
// exist. scope "" means no scope is needed; a nil pair means the base creator
// check (project, manage) is sufficient.
type tool struct {
	name        string
	description string
	scope       string
	pair        func(projectID int64) string
	action      string
	// add registers the tool on the server.
	add func(s *mcpsdk.Server, h *handler, t tool)
}

// handle adapts a typed tool function to the SDK. The SDK infers the input
// schema from In and refuses arguments that do not match it before run is
// called. The caller comes from the request, not from the server.
func handle[In any](run func(ctx context.Context, h *handler, p Principal, in In) (any, error)) func(*mcpsdk.Server, *handler, tool) {
	return func(s *mcpsdk.Server, h *handler, t tool) {
		mcpsdk.AddTool(s, &mcpsdk.Tool{Name: t.name, Description: t.description},
			func(ctx context.Context, req *mcpsdk.CallToolRequest, in In) (*mcpsdk.CallToolResult, any, error) {
				p, ok := principalFrom(req.Extra)
				if !ok {
					return nil, nil, errInternal
				}
				out, err := h.guard(ctx, p, t, func() (any, error) { return run(ctx, h, p, in) })
				return nil, out, err
			})
	}
}

// Tool inputs. A field without omitempty is required.
type (
	noInput              struct{}
	listLeaderboardsArgs struct {
		IncludeDeleted bool `json:"include_deleted,omitempty" jsonschema:"also list deleted leaderboards that can be restored"`
	}
	leaderboardArgs struct {
		LeaderboardID int64 `json:"leaderboard_id" jsonschema:"leaderboard ID"`
	}
	listRevisionsArgs struct {
		Resource      string `json:"resource" jsonschema:"remote_config or leaderboard"`
		LeaderboardID int64  `json:"leaderboard_id,omitempty" jsonschema:"required when resource is leaderboard"`
	}
	ticketTraceArgs struct {
		TicketID int64 `json:"ticket_id,omitempty" jsonschema:"matchmaking ticket ID"`
		PlayerID int64 `json:"player_id,omitempty" jsonschema:"player ID; finds the player's queued ticket"`
	}
)

// tools is the tool permission table. Descriptions are constants in the
// binary: no tool description comes from stored data.
var tools = []tool{
	{
		name: "project_health_check",
		description: "Find setup problems in this Game Project: API keys, key scopes, feature grants, " +
			"server feature switches, relay, CORS origins, Steam sign-in, remote config size, and quota state. " +
			"Tenant-level quotas are reported as ok, near_limit, or at_limit only.",
		add: handle(projectHealthCheck),
	},
	{
		name: "matchmaking_ticket_trace",
		description: "Show the state of one matchmaking ticket. Give ticket_id (any state) or player_id " +
			"(only the ticket the player has in the queue now). Finished tickets are kept for 24 hours. " +
			"To see a finished ticket, use the ticket ID the API returned when the game made the ticket. " +
			"Text the game client sent is shown only if it is a short identifier; other text is replaced by a marker " +
			"with its length. The attributes blob is never shown, only its size.",
		pair:   rbac.ProjectMatchmakerObject,
		action: rbac.ActionRead,
		add:    handle(matchmakingTicketTrace),
	},
	{
		name:        "get_remote_config",
		description: "Get the remote config JSON of this Game Project and its revision number (0 if it has no revision).",
		add:         handle(getRemoteConfig),
	},
	{
		name:        "list_leaderboards",
		description: "List leaderboard settings of this Game Project (no scores). Set include_deleted to also list deleted leaderboards that can be restored.",
		add:         handle(listLeaderboards),
	},
	{
		name:        "get_leaderboard",
		description: "Get the settings of one leaderboard (no scores) and its revision number.",
		add:         handle(getLeaderboard),
	},
	{
		name:        "list_api_keys",
		description: "List the active API keys that work for this Game Project: label, type, scopes, creation time. Key values are never shown.",
		add:         handle(listAPIKeys),
	},
	{
		name:        "list_revisions",
		description: "List the kept revisions (newest 3) of the remote config or of one leaderboard.",
		add:         handle(listRevisions),
	},
	{
		name: "set_remote_config",
		description: "Replace the remote config of this Game Project. Give the full config as a JSON object string and " +
			"the revision number from get_remote_config. A stale revision is refused. Undo with rollback_remote_config.",
		scope: ScopeConfigWrite, pair: rbac.ProjectConfigObject, action: rbac.ActionUpdate,
		add: handle(setRemoteConfig),
	},
	{
		name: "rollback_remote_config",
		description: "Apply a kept revision of the remote config as a new revision. Use list_revisions to find one. " +
			"expected_revision must be the newest revision number.",
		scope: ScopeConfigWrite, pair: rbac.ProjectConfigObject, action: rbac.ActionUpdate,
		add: handle(rollbackRemoteConfig),
	},
	{
		name:        "create_leaderboard",
		description: "Create a leaderboard in this Game Project. score_operator is fixed after creation.",
		scope:       ScopeLeaderboardsWrite, pair: rbac.ProjectLeaderboardObject, action: rbac.ActionManage,
		add: handle(createLeaderboard),
	},
	{
		name: "update_leaderboard",
		description: "Replace the settings of a leaderboard. Send all settings: an optional setting that is not sent " +
			"is removed. Read them with get_leaderboard first and give its revision. Undo with rollback_leaderboard.",
		scope: ScopeLeaderboardsWrite, pair: rbac.ProjectLeaderboardObject, action: rbac.ActionManage,
		add: handle(updateLeaderboard),
	},
	{
		name: "rollback_leaderboard",
		description: "Apply a kept revision of a leaderboard's settings as a new revision. Scores do not change. " +
			"expected_revision must be the newest revision number.",
		scope: ScopeLeaderboardsWrite, pair: rbac.ProjectLeaderboardObject, action: rbac.ActionManage,
		add: handle(rollbackLeaderboard),
	},
	{
		name:        "delete_leaderboard",
		description: "Delete a leaderboard. Scores are kept, and restore_leaderboard undoes the delete.",
		scope:       ScopeLeaderboardsWrite, pair: rbac.ProjectLeaderboardObject, action: rbac.ActionManage,
		add: handle(deleteLeaderboard),
	},
	{
		name: "restore_leaderboard",
		description: "Restore a deleted leaderboard with its scores. Use list_leaderboards with include_deleted to find it. " +
			"Refused if a live leaderboard in this Game Project has the same name.",
		scope: ScopeLeaderboardsWrite, pair: rbac.ProjectLeaderboardObject, action: rbac.ActionManage,
		add: handle(restoreLeaderboard),
	},
	{
		name: "create_api_key",
		description: "Create a publishable (ggp_) API key for this Game Project, to ship in a game client. Secret keys " +
			"cannot be made here. The value is returned once. Scopes are limited to the features this Game Project has.",
		scope: ScopeKeysCreate, pair: func(int64) string { return rbac.ObjectAPIKeyPublic }, action: rbac.ActionManage,
		add: handle(createAPIKey),
	},
	{
		name: "set_allowed_origins",
		description: "Replace the browser origins of this Game Project (CORS for web builds). Give the full list. " +
			"The result has the old list, so a wrong change can be put back.",
		scope: ScopeOriginsWrite, pair: rbac.ProjectConfigObject, action: rbac.ActionUpdate,
		add: handle(setAllowedOrigins),
	},
}

// newServer builds the SDK server with all tools. A call to a tool outside
// the token's scopes gets a message that names the scope; tools/list shows
// only the tools that the caller's scopes permit.
func (h *handler) newServer() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: serverName, Version: h.d.Version},
		&mcpsdk.ServerOptions{Instructions: serverInstructions})
	for _, t := range tools {
		t.add(s, h, t)
	}
	s.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			res, err := next(ctx, method, req)
			list, ok := res.(*mcpsdk.ListToolsResult)
			if !ok || err != nil {
				return res, err
			}
			p, _ := principalFrom(req.GetExtra())
			list.Tools = permittedTools(list.Tools, p)
			return list, nil
		}
	})
	return s
}

// permittedTools keeps the tools whose scope the token has. It does not check
// the creator pair, so a listed tool can still be refused.
func permittedTools(all []*mcpsdk.Tool, p Principal) []*mcpsdk.Tool {
	out := make([]*mcpsdk.Tool, 0, len(all))
	for _, t := range all {
		row, ok := toolByName[t.Name]
		if ok && (row.scope == "" || p.HasScope(row.scope)) {
			out = append(out, t)
		}
	}
	return out
}

var toolByName = func() map[string]tool {
	m := make(map[string]tool, len(tools))
	for _, t := range tools {
		m[t.name] = t
	}
	return m
}()

// userError is a message that is safe to show the agent.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func errorf(format string, args ...any) error { return userError{fmt.Sprintf(format, args...)} }

var errNotFound = userError{"not found in this Game Project"}

var errInternal = errors.New("internal error")

// guard runs the scope check and the creator pair check, then the tool. A
// returned error becomes a tool result with isError. Only userError text
// reaches the agent; other errors are logged and reported as internal.
func (h *handler) guard(ctx context.Context, p Principal, t tool, run func() (any, error)) (any, error) {
	if t.scope != "" && !p.HasScope(t.scope) {
		return nil, errorf("This token does not have the %q scope. Create a token with that scope in the dashboard.", t.scope)
	}
	if t.pair != nil {
		ok, err := h.d.RBAC.CanControlPanel(p.CreatedByUserID, p.TenantID, t.pair(p.ProjectID), t.action)
		if err != nil {
			slog.ErrorContext(ctx, "mcp: creator pair check", "tool", t.name, "err", err)
			return nil, errInternal
		}
		if !ok {
			return nil, errorf("The person who created this token does not have the %s permission on %s, so this tool is refused.",
				t.action, t.pair(p.ProjectID))
		}
	}
	out, err := run()
	var ue userError
	if err != nil && !errors.As(err, &ue) {
		slog.ErrorContext(ctx, "mcp: tool failed", "tool", t.name, "err", err)
		return nil, errInternal
	}
	return out, err
}
