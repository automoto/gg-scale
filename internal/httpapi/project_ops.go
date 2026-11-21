package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/mcp"
	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
)

const projectManagementTag = "Project Management"

// managementTokenSecurity is the requirement for each /v1/project operation.
var managementTokenSecurity = []map[string][]string{{"ManagementToken": {}}}

// Operation.Metadata keys of a /v1/project operation. The MCP tool filter
// reads them: managementToolMetadata marks an operation as a row of
// projectOps (its value is the tool name), managementScopeMetadata is the
// write scope the row needs ("" for none).
const (
	managementToolMetadata  = "managementTool"
	managementScopeMetadata = "managementScope"
)

// projectOp is one row of the permission table: one /v1/project operation
// and one MCP tool. An operation with no row does not exist. scope "" means
// no scope is needed. A row with a scope uses the creator pair of that scope
// in managementtoken.WriteScopes. pair and action are only for a row with no
// scope that needs more than the base creator check of the token middleware
// (project, manage).
type projectOp struct {
	// tool and description are the MCP tool name and description. The
	// descriptions are constants in the binary: none comes from stored data.
	tool        string
	description string
	scope       string
	pair        func(projectID int64) string
	action      string
	// op holds the method, path, operation ID, summary, and description for
	// the API reference. The register helper adds the rest.
	op      huma.Operation
	handler func(api huma.API, d Deps, row projectOp)
}

// registerProjectManagementRoutes registers each row of projectOps. It reads
// no Deps value here, so OpenAPIDoc can call it with zero Deps.
func registerProjectManagementRoutes(api huma.API, d Deps) {
	for _, row := range projectOps {
		row.handler(api, d, row)
	}
}

// operation returns a copy of the row's operation with the tag, the
// security requirement, the metadata, and the MCP tool mark. The row stays
// unchanged, because huma fills in the operation it registers.
func (row projectOp) operation() huma.Operation {
	op := row.op
	op.Tags = []string{projectManagementTag}
	op.Security = managementTokenSecurity
	op.Metadata = map[string]any{
		managementToolMetadata:  row.tool,
		managementScopeMetadata: row.scope,
	}
	op.MaxBodyBytes = 131072
	return mcp.WithTool(op, mcp.WithName(row.tool), mcp.WithDescription(row.description))
}

// projectOutput is the response of each /v1/project operation: 200 with a
// JSON object.
type projectOutput[B any] struct {
	Body B
}

// projectHandler adapts a typed handler to a row. The wrapper runs the
// scope check and the creator pair check before the handler. Only huma
// status errors reach the client; other errors are logged and become a 500
// with the text "internal error".
func projectHandler[I, B any](run func(ctx context.Context, d Deps, p managementtoken.Principal, in *I) (B, error)) func(huma.API, Deps, projectOp) {
	return func(api huma.API, d Deps, row projectOp) {
		huma.Register(api, row.operation(), func(ctx context.Context, in *I) (*projectOutput[B], error) {
			p, ok := managementtoken.FromContext(ctx)
			if !ok {
				return nil, huma.Error401Unauthorized("unauthorized")
			}
			if err := row.authorize(ctx, d, p); err != nil {
				return nil, err
			}
			body, err := run(ctx, d, p, in)
			var se huma.StatusError
			if err != nil && !errors.As(err, &se) {
				return nil, serverError(ctx, "project management: "+row.tool, err)
			}
			if err != nil {
				return nil, err
			}
			return &projectOutput[B]{Body: body}, nil
		})
	}
}

// authorize runs the scope check, then the creator pair check.
func (row projectOp) authorize(ctx context.Context, d Deps, p managementtoken.Principal) error {
	pair, action := row.pair, row.action
	if row.scope != "" {
		if !p.HasScope(row.scope) {
			return huma.Error403Forbidden(fmt.Sprintf(
				"This token does not have the %q scope. Create a token with that scope in the dashboard.", row.scope))
		}
		sp, ok := managementtoken.WriteScope(row.scope)
		if !ok {
			return serverError(ctx, "project management: unknown scope: "+row.tool, errors.New(row.scope))
		}
		pair, action = sp.Pair, sp.Action
	}
	if pair == nil {
		return nil
	}
	obj := pair(p.ProjectID)
	ok, err := d.RBAC.CanControlPanel(p.CreatedByUserID, p.TenantID, obj, action)
	if err != nil {
		return serverError(ctx, "project management: creator pair check: "+row.tool, err)
	}
	if !ok {
		return huma.Error403Forbidden(fmt.Sprintf(
			"The person who created this token does not have the %s permission on %s, so this request is refused.",
			action, obj))
	}
	return nil
}

const projectNotFoundText = "not found in this Game Project"

func errProjectNotFound() error { return huma.Error404NotFound(projectNotFoundText) }

// projectWriteError maps the errors of the projectadmin writes to safe
// client errors. Other errors are returned unchanged and become a 500.
func projectWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, projectadmin.ErrProjectNotInTenant):
		return errProjectNotFound()
	case errors.Is(err, projectadmin.ErrRevisionConflict):
		return huma.Error409Conflict("expected_revision is not the newest revision. Read the resource again and retry with its revision.")
	case errors.Is(err, projectadmin.ErrRevisionNotFound):
		return huma.Error404NotFound("that revision is not kept. Use list_revisions to see the kept revisions.")
	case errors.Is(err, projectadmin.ErrDuplicateLeaderboard):
		return huma.Error409Conflict("a leaderboard with that name already exists in this Game Project")
	case errors.Is(err, projectadmin.ErrSortOrderLocked):
		return huma.Error409Conflict("sort_order cannot change after the leaderboard has scores")
	case errors.Is(err, projectadmin.ErrLeaderboardLimit):
		return huma.Error409Conflict("this Game Project already has the maximum number of leaderboards. Delete one, or ask a platform admin to raise the limit.")
	case errors.Is(err, projectadmin.ErrAPIKeyLimit):
		return huma.Error409Conflict("this Game Project already has the maximum number of active API keys. Revoke one in the dashboard.")
	case errors.Is(err, projectadmin.ErrOriginLimit):
		// Before ErrInvalidOrigins, which it wraps.
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, projectadmin.ErrInvalidLeaderboard),
		errors.Is(err, projectadmin.ErrInvalidOrigins),
		errors.Is(err, projectadmin.ErrScopeNotGrantable):
		return huma.Error422UnprocessableEntity(err.Error())
	}
	return err
}

// projectOps is the permission table.
var projectOps = []projectOp{
	{
		tool: "project_health_check",
		description: "Find setup problems in this Game Project: API keys, key scopes, feature grants, " +
			"server feature switches, relay, CORS origins, Steam sign-in, remote config size, and quota state. " +
			"Tenant-level quotas are reported as ok, near_limit, or at_limit only.",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/health", OperationID: "getProjectHealth",
			Summary: "Check the Game Project setup",
			Description: "Returns a list of setup problems and the facts they come from: API keys, feature grants, " +
				"server feature switches, CORS origins, Steam sign-in, remote config size, and quota state. " +
				"Tenant-level quotas show only ok, near_limit, or at_limit.",
		},
		handler: projectHandler(projectHealthCheck),
	},
	{
		tool: "matchmaking_ticket_trace",
		description: "Show the state of one matchmaking ticket. Give ticket_id (any state) or player_id " +
			"(only the ticket the player has in the queue now). Finished tickets are kept for 24 hours. " +
			"To see a finished ticket, use the ticket ID the API returned when the game made the ticket. " +
			"Text the game client sent is shown only if it is a short identifier; other text is replaced by a marker " +
			"with its length. The attributes blob is never shown, only its size.",
		pair:   rbac.ProjectMatchmakerObject,
		action: rbac.ActionRead,
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/matchmaking/ticket-trace", OperationID: "getProjectTicketTrace",
			Summary: "Trace a matchmaking ticket",
			Description: "Returns the state of one matchmaking ticket. Give exactly one of ticket_id (any state) or " +
				"player_id (the ticket the player has in the queue now). Finished tickets are kept for 24 hours. " +
				"Text that the game client sent is shown only when it is a short identifier; other text is replaced " +
				"by a marker with its length. The attributes are never shown, only their size.",
		},
		handler: projectHandler(matchmakingTicketTrace),
	},
	{
		tool:        "get_remote_config",
		description: "Get the remote config JSON of this Game Project and its revision number (0 if it has no revision).",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/remote-config", OperationID: "getProjectRemoteConfig",
			Summary:     "Get the remote config",
			Description: "Returns the remote config of the Game Project and its revision number (0 if it has no revision).",
		},
		handler: projectHandler(getRemoteConfig),
	},
	{
		tool: "set_remote_config",
		description: "Replace the remote config of this Game Project. Give the full config as a JSON object and " +
			"the revision number from get_remote_config. A stale revision is refused. Undo with rollback_remote_config.",
		scope: managementtoken.ScopeConfigWrite,
		op: huma.Operation{
			Method: http.MethodPut, Path: "/v1/project/remote-config", OperationID: "setProjectRemoteConfig",
			Summary: "Replace the remote config",
			Description: "Replaces the remote config with a JSON object of up to 64 KiB. expected_revision must be " +
				"the newest revision number; a stale revision answers 409. Each change makes a new revision.",
		},
		handler: projectHandler(setRemoteConfig),
	},
	{
		tool: "rollback_remote_config",
		description: "Apply a kept revision of the remote config as a new revision. Use list_revisions to find one. " +
			"expected_revision must be the newest revision number.",
		scope: managementtoken.ScopeConfigWrite,
		op: huma.Operation{
			Method: http.MethodPost, Path: "/v1/project/remote-config/rollback", OperationID: "rollbackProjectRemoteConfig",
			Summary: "Roll back the remote config",
			Description: "Applies a kept revision of the remote config as a new revision. expected_revision must be " +
				"the newest revision number; a stale revision answers 409.",
		},
		handler: projectHandler(rollbackRemoteConfig),
	},
	{
		tool:        "list_leaderboards",
		description: "List leaderboard settings of this Game Project (no scores). Set include_deleted to also list deleted leaderboards that can be restored.",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/leaderboards", OperationID: "listProjectLeaderboards",
			Summary:     "List leaderboards",
			Description: "Returns the settings of the leaderboards (no scores), at most 50. Set include_deleted to also list the deleted leaderboards that can be restored.",
		},
		handler: projectHandler(listLeaderboards),
	},
	{
		tool:        "get_leaderboard",
		description: "Get the settings of one leaderboard (no scores) and its revision number.",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/leaderboards/{leaderboard_id}", OperationID: "getProjectLeaderboard",
			Summary:     "Get a leaderboard",
			Description: "Returns the settings of one leaderboard (no scores) and its revision number.",
		},
		handler: projectHandler(getLeaderboard),
	},
	{
		tool:        "create_leaderboard",
		description: "Create a leaderboard in this Game Project. score_operator is fixed after creation.",
		scope:       managementtoken.ScopeLeaderboardsWrite,
		op: huma.Operation{
			Method: http.MethodPost, Path: "/v1/project/leaderboards", OperationID: "createProjectLeaderboard",
			Summary:     "Create a leaderboard",
			Description: "Creates a leaderboard at revision 1. score_operator is fixed after creation.",
		},
		handler: projectHandler(createLeaderboard),
	},
	{
		tool: "update_leaderboard",
		description: "Replace the settings of a leaderboard. Send all settings: an optional setting that is not sent " +
			"is removed. Read them with get_leaderboard first and give its revision. Undo with rollback_leaderboard.",
		scope: managementtoken.ScopeLeaderboardsWrite,
		op: huma.Operation{
			Method: http.MethodPut, Path: "/v1/project/leaderboards/{leaderboard_id}", OperationID: "updateProjectLeaderboard",
			Summary: "Replace leaderboard settings",
			Description: "Replaces all settings of a leaderboard: an optional setting that is not sent is removed. " +
				"expected_revision must be the newest revision number; a stale revision answers 409.",
		},
		handler: projectHandler(updateLeaderboard),
	},
	{
		tool: "rollback_leaderboard",
		description: "Apply a kept revision of a leaderboard's settings as a new revision. Scores do not change. " +
			"expected_revision must be the newest revision number.",
		scope: managementtoken.ScopeLeaderboardsWrite,
		op: huma.Operation{
			Method: http.MethodPost, Path: "/v1/project/leaderboards/{leaderboard_id}/rollback", OperationID: "rollbackProjectLeaderboard",
			Summary: "Roll back leaderboard settings",
			Description: "Applies a kept revision of the leaderboard settings as a new revision. Scores do not change. " +
				"expected_revision must be the newest revision number; a stale revision answers 409.",
		},
		handler: projectHandler(rollbackLeaderboard),
	},
	{
		tool:        "delete_leaderboard",
		description: "Delete a leaderboard. Scores are kept, and restore_leaderboard undoes the delete.",
		scope:       managementtoken.ScopeLeaderboardsWrite,
		op: huma.Operation{
			Method: http.MethodDelete, Path: "/v1/project/leaderboards/{leaderboard_id}", OperationID: "deleteProjectLeaderboard",
			Summary:     "Delete a leaderboard",
			Description: "Deletes a leaderboard. The scores are kept, and the restore operation undoes the delete.",
		},
		handler: projectHandler(deleteLeaderboard),
	},
	{
		tool: "restore_leaderboard",
		description: "Restore a deleted leaderboard with its scores. Use list_leaderboards with include_deleted to find it. " +
			"Refused if a live leaderboard in this Game Project has the same name.",
		scope: managementtoken.ScopeLeaderboardsWrite,
		op: huma.Operation{
			Method: http.MethodPost, Path: "/v1/project/leaderboards/{leaderboard_id}/restore", OperationID: "restoreProjectLeaderboard",
			Summary: "Restore a deleted leaderboard",
			Description: "Restores a deleted leaderboard with its scores. Answers 409 when a live leaderboard has " +
				"the same name.",
		},
		handler: projectHandler(restoreLeaderboard),
	},
	{
		tool:        "list_api_keys",
		description: "List the active API keys that work for this Game Project: label, type, scopes, creation time. Key values are never shown.",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/api-keys", OperationID: "listProjectAPIKeys",
			Summary:     "List API keys",
			Description: "Returns the active API keys that work for the Game Project, at most 50: label, type, scopes, and creation time. Key values are never returned.",
		},
		handler: projectHandler(listAPIKeys),
	},
	{
		tool: "create_api_key",
		description: "Create a publishable (ggp_) API key for this Game Project, to ship in a game client. Secret keys " +
			"cannot be made here. The value is returned once. Scopes are limited to the features this Game Project has.",
		scope: managementtoken.ScopeKeysCreate,
		op: huma.Operation{
			Method: http.MethodPost, Path: "/v1/project/api-keys", OperationID: "createProjectAPIKey",
			Summary: "Create a publishable API key",
			Description: "Creates a publishable (ggp_) API key for the Game Project. Secret keys cannot be made here. " +
				"The key value is returned only in this response. Scopes are limited to the features of the Game Project. " +
				"Answers 409 when the Game Project has the maximum number of active keys.",
		},
		handler: projectHandler(createAPIKey),
	},
	{
		tool:        "list_revisions",
		description: "List the kept revisions (newest 3) of the remote config or of one leaderboard.",
		op: huma.Operation{
			Method: http.MethodGet, Path: "/v1/project/revisions", OperationID: "listProjectRevisions",
			Summary:     "List kept revisions",
			Description: "Returns the kept revisions (newest 3) of the remote config or of one leaderboard, newest first.",
		},
		handler: projectHandler(listRevisions),
	},
	{
		tool: "set_allowed_origins",
		description: "Replace the browser origins of this Game Project (CORS for web builds). Give the full list. " +
			"The result has the old list, so a wrong change can be put back.",
		scope: managementtoken.ScopeOriginsWrite,
		op: huma.Operation{
			Method: http.MethodPut, Path: "/v1/project/allowed-origins", OperationID: "setProjectAllowedOrigins",
			Summary: "Replace the allowed browser origins",
			Description: "Replaces the browser origins (CORS for web builds) of the Game Project with the full list. " +
				"The response has the old list. Answers 409 when the list has more origins than the limit.",
		},
		handler: projectHandler(setAllowedOrigins),
	},
}
