package mcp

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/tenant"
)

// actor records a tool write as the token, not as the person who made it.
func (p Principal) actor() projectadmin.Actor {
	return projectadmin.Actor{TokenID: p.TokenID, TokenCreatorID: p.CreatedByUserID}
}

// writeError turns the errors of the shared write functions into messages
// that are safe to show the agent. Other errors stay internal.
func writeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return errNotFound
	case errors.Is(err, projectadmin.ErrRevisionConflict):
		return errorf("expected_revision is not the newest revision. Read the resource again and retry with its revision.")
	case errors.Is(err, projectadmin.ErrRevisionNotFound):
		return errorf("that revision is not kept. Use list_revisions to see the kept revisions.")
	case errors.Is(err, projectadmin.ErrDuplicateLeaderboard):
		return errorf("a leaderboard with that name already exists in this Game Project")
	case errors.Is(err, projectadmin.ErrSortOrderLocked):
		return errorf("sort_order cannot change after the leaderboard has scores")
	case errors.Is(err, projectadmin.ErrInvalidLeaderboard),
		errors.Is(err, projectadmin.ErrInvalidOrigins),
		errors.Is(err, projectadmin.ErrScopeNotGrantable):
		return userError{err.Error()}
	case errors.Is(err, projectadmin.ErrAPIKeyLimit):
		return errorf("this Game Project already has the maximum number of active API keys. Revoke one in the dashboard.")
	}
	return err
}

// Tool inputs. JSON objects (remote config, leaderboard metadata) come as
// strings: the SDK decodes object arguments into float64 numbers, which
// would change large integers. The server parses the strings with exact
// numbers, the same as the dashboard.
type (
	setRemoteConfigArgs struct {
		ConfigJSON       string `json:"config_json" jsonschema:"the full remote config, a JSON object encoded as a string, up to 64 KiB"`
		ExpectedRevision int64  `json:"expected_revision" jsonschema:"the newest revision number from get_remote_config (0 if it has no revision)"`
	}
	rollbackRemoteConfigArgs struct {
		Revision         int64 `json:"revision" jsonschema:"the kept revision to apply"`
		ExpectedRevision int64 `json:"expected_revision" jsonschema:"the newest revision number"`
	}
	createLeaderboardArgs struct {
		Name              string `json:"name" jsonschema:"1 to 120 characters"`
		SortOrder         string `json:"sort_order,omitempty" jsonschema:"desc (high score first, default) or asc"`
		ScoreOperator     string `json:"score_operator,omitempty" jsonschema:"best (default), set, or incr; fixed after creation"`
		ClientSubmissions bool   `json:"client_submissions,omitempty" jsonschema:"let game clients submit scores with a publishable key"`
		ScoreMin          *int64 `json:"score_min,omitempty"`
		ScoreMax          *int64 `json:"score_max,omitempty"`
		ResetSchedule     string `json:"reset_schedule,omitempty" jsonschema:"none (default), daily, weekly, or monthly"`
		AttemptCap        *int32 `json:"attempt_cap,omitempty" jsonschema:"maximum submissions per player per period"`
		MetadataJSON      string `json:"metadata_json,omitempty" jsonschema:"display metadata, a JSON object encoded as a string, up to 16 KiB"`
	}
	updateLeaderboardArgs struct {
		LeaderboardID     int64  `json:"leaderboard_id"`
		ExpectedRevision  int64  `json:"expected_revision" jsonschema:"the revision number from get_leaderboard"`
		Name              string `json:"name"`
		SortOrder         string `json:"sort_order,omitempty" jsonschema:"desc (default) or asc"`
		ClientSubmissions bool   `json:"client_submissions,omitempty"`
		ScoreMin          *int64 `json:"score_min,omitempty"`
		ScoreMax          *int64 `json:"score_max,omitempty"`
		ResetSchedule     string `json:"reset_schedule,omitempty" jsonschema:"none (default), daily, weekly, or monthly"`
		AttemptCap        *int32 `json:"attempt_cap,omitempty"`
		MetadataJSON      string `json:"metadata_json,omitempty" jsonschema:"a JSON object encoded as a string, up to 16 KiB"`
	}
	rollbackLeaderboardArgs struct {
		LeaderboardID    int64 `json:"leaderboard_id"`
		Revision         int64 `json:"revision" jsonschema:"the kept revision to apply"`
		ExpectedRevision int64 `json:"expected_revision" jsonschema:"the newest revision number"`
	}
	createAPIKeyArgs struct {
		Label  string   `json:"label" jsonschema:"1 to 64 characters"`
		Scopes []string `json:"scopes,omitempty" jsonschema:"extra feature scopes: fleet or p2p_relay; matchmaker is always added"`
	}
	setAllowedOriginsArgs struct {
		Origins []string `json:"origins" jsonschema:"the full list of browser origins, for example https://html-classic.itch.zone or http://localhost:5173"`
	}
)

func setRemoteConfig(ctx context.Context, h *handler, p Principal, in setRemoteConfigArgs) (any, error) {
	config, err := projectadmin.NormalizeJSONObject(in.ConfigJSON, projectadmin.RemoteConfigMaxBytes)
	if err != nil {
		return nil, errorf("config_json: %v", err)
	}
	rev, err := projectadmin.SetRemoteConfig(ctx, h.d.Pool, p.TenantID, p.ProjectID, config, &in.ExpectedRevision, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"revision": rev}, nil
}

func rollbackRemoteConfig(ctx context.Context, h *handler, p Principal, in rollbackRemoteConfigArgs) (any, error) {
	rev, err := projectadmin.RollbackRemoteConfig(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.Revision, &in.ExpectedRevision, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"revision": rev}, nil
}

// leaderboardSettings fills defaults and parses the metadata string.
func leaderboardSettings(name, sortOrder, operator, schedule, metadata string, client bool, minScore, maxScore *int64, attemptCap *int32) (projectadmin.LeaderboardSettings, error) {
	s := projectadmin.LeaderboardSettings{
		Name: name, SortOrder: sortOrder, ScoreOperator: operator, ResetSchedule: schedule,
		ClientSubmissions: client, ScoreMin: minScore, ScoreMax: maxScore, AttemptCap: attemptCap,
	}
	if s.SortOrder == "" {
		s.SortOrder = "desc"
	}
	if s.ScoreOperator == "" {
		s.ScoreOperator = "best"
	}
	if s.ResetSchedule == "" {
		s.ResetSchedule = "none"
	}
	if metadata != "" {
		blob, err := projectadmin.NormalizeJSONObject(metadata, projectadmin.LeaderboardMetadataMaxBytes)
		if err != nil {
			return s, errorf("metadata_json: %v", err)
		}
		s.Metadata = blob
	}
	return s, nil
}

func createLeaderboard(ctx context.Context, h *handler, p Principal, in createLeaderboardArgs) (any, error) {
	s, err := leaderboardSettings(in.Name, in.SortOrder, in.ScoreOperator, in.ResetSchedule, in.MetadataJSON,
		in.ClientSubmissions, in.ScoreMin, in.ScoreMax, in.AttemptCap)
	if err != nil {
		return nil, err
	}
	if err := projectadmin.ValidateLeaderboard(s, true); err != nil {
		return nil, writeError(err)
	}
	id, err := projectadmin.CreateLeaderboard(ctx, h.d.Pool, p.TenantID, p.ProjectID, s, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"leaderboard_id": id, "revision": 1}, nil
}

func updateLeaderboard(ctx context.Context, h *handler, p Principal, in updateLeaderboardArgs) (any, error) {
	s, err := leaderboardSettings(in.Name, in.SortOrder, "", in.ResetSchedule, in.MetadataJSON,
		in.ClientSubmissions, in.ScoreMin, in.ScoreMax, in.AttemptCap)
	if err != nil {
		return nil, err
	}
	if err := projectadmin.ValidateLeaderboard(s, false); err != nil {
		return nil, writeError(err)
	}
	rev, err := projectadmin.UpdateLeaderboard(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, s, &in.ExpectedRevision, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"revision": rev}, nil
}

func rollbackLeaderboard(ctx context.Context, h *handler, p Principal, in rollbackLeaderboardArgs) (any, error) {
	rev, err := projectadmin.RollbackLeaderboard(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, in.Revision, &in.ExpectedRevision, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"revision": rev, "note": "Settings were rolled back. Scores did not change."}, nil
}

func deleteLeaderboard(ctx context.Context, h *handler, p Principal, in leaderboardArgs) (any, error) {
	if err := projectadmin.DeleteLeaderboard(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, p.actor()); err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"deleted": true, "note": "Scores are kept. Use restore_leaderboard to undo."}, nil
}

func restoreLeaderboard(ctx context.Context, h *handler, p Principal, in leaderboardArgs) (any, error) {
	if err := projectadmin.RestoreLeaderboard(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, p.actor()); err != nil {
		return nil, writeError(err)
	}
	return map[string]any{"restored": true}, nil
}

const apiKeyLabelMax = 64

func createAPIKey(ctx context.Context, h *handler, p Principal, in createAPIKeyArgs) (any, error) {
	if in.Label == "" || !utf8.ValidString(in.Label) || utf8.RuneCountInString(in.Label) > apiKeyLabelMax {
		return nil, errorf("label must be 1 to %d characters", apiKeyLabelMax)
	}
	for _, r := range in.Label {
		if unicode.IsControl(r) {
			return nil, errorf("label must not contain control characters")
		}
	}
	// The token label goes into the key label, so a person can find the
	// key in the dashboard and revoke it.
	label := fmt.Sprintf("%s (MCP token: %s)", in.Label, p.Label)
	projectID := p.ProjectID
	id, value, err := projectadmin.CreateAPIKey(ctx, h.d.Pool, h.d.RBAC, p.TenantID, projectadmin.NewAPIKey{
		ProjectID: &projectID,
		Label:     label,
		Type:      tenant.KeyTypePublishable,
		Scopes:    in.Scopes,
		MaxActive: h.d.MaxProjectAPIKeys,
		Switches:  projectadmin.KeySwitches{FleetEnabled: h.d.FleetEnabled, RelayEnabled: h.d.RelayEnabled},
	}, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	return map[string]any{
		"api_key_id": id,
		"api_key":    value,
		"key_type":   tenant.KeyTypePublishable,
		"label":      label,
		"note": "A publishable key is made to ship in a game client. The value is shown only now. " +
			"Revoke it in the dashboard if it is not needed.",
	}, nil
}

func setAllowedOrigins(ctx context.Context, h *handler, p Principal, in setAllowedOriginsArgs) (any, error) {
	old, err := projectadmin.SetAllowedOrigins(ctx, h.d.Pool, p.TenantID, p.ProjectID, in.Origins, h.d.MaxProjectOrigins, p.actor())
	if err != nil {
		return nil, writeError(err)
	}
	now, err := projectadmin.AllowedOrigins(ctx, h.d.Pool, p.TenantID, p.ProjectID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"origins":     now,
		"old_origins": old,
		"note": "To undo, call set_allowed_origins with old_origins. Each server uses the new list within " +
			projectadmin.OriginCacheTTL.String() + ".",
	}, nil
}
