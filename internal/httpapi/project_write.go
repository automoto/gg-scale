package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

const projectAPIKeyLabelMax = 64

// projectActor records a write as the token, not as the person who made it.
func projectActor(p managementtoken.Principal) projectadmin.Actor {
	return projectadmin.Actor{TokenID: p.TokenID, TokenCreatorID: p.CreatedByUserID}
}

type projectRevisionResult struct {
	Revision int64 `json:"revision" doc:"The new revision number." example:"4"`
}

type projectSetRemoteConfigBody struct {
	Config           json.RawMessage `json:"config" doc:"The full remote config, a JSON object, up to 64 KiB."`
	ExpectedRevision int64           `json:"expected_revision" doc:"The newest revision number from the remote config read (0 if it has no revision)." example:"3"`
}

type projectSetRemoteConfigInput struct {
	Body projectSetRemoteConfigBody
}

func setRemoteConfig(ctx context.Context, d Deps, p managementtoken.Principal, in *projectSetRemoteConfigInput) (projectRevisionResult, error) {
	config, err := projectadmin.NormalizeJSONObject(string(in.Body.Config), projectadmin.RemoteConfigMaxBytes)
	if err != nil {
		return projectRevisionResult{}, huma.Error422UnprocessableEntity("config: " + err.Error())
	}
	rev, err := projectadmin.SetRemoteConfig(ctx, d.Pool, p.TenantID, p.ProjectID, config, &in.Body.ExpectedRevision, projectActor(p))
	if err != nil {
		return projectRevisionResult{}, projectWriteError(err)
	}
	return projectRevisionResult{Revision: rev}, nil
}

type projectRollbackBody struct {
	Revision         int64 `json:"revision" doc:"The kept revision to apply." example:"2"`
	ExpectedRevision int64 `json:"expected_revision" doc:"The newest revision number." example:"3"`
}

type projectRollbackRemoteConfigInput struct {
	Body projectRollbackBody
}

func rollbackRemoteConfig(ctx context.Context, d Deps, p managementtoken.Principal, in *projectRollbackRemoteConfigInput) (projectRevisionResult, error) {
	rev, err := projectadmin.RollbackRemoteConfig(ctx, d.Pool, p.TenantID, p.ProjectID, in.Body.Revision, &in.Body.ExpectedRevision, projectActor(p))
	if err != nil {
		return projectRevisionResult{}, projectWriteError(err)
	}
	return projectRevisionResult{Revision: rev}, nil
}

// ProjectLeaderboardFields are the leaderboard settings of the create and
// the update body. huma merges an embedded struct into the body schema only
// when its type is exported.
type ProjectLeaderboardFields struct {
	Name              string          `json:"name" doc:"1 to 120 characters." example:"weekly-high-scores"`
	SortOrder         string          `json:"sort_order,omitempty" doc:"desc (high score first, default) or asc." example:"desc"`
	ClientSubmissions bool            `json:"client_submissions,omitempty" doc:"Let game clients submit scores with a publishable key."`
	ScoreMin          *int64          `json:"score_min,omitempty"`
	ScoreMax          *int64          `json:"score_max,omitempty"`
	ResetSchedule     string          `json:"reset_schedule,omitempty" doc:"none (default), daily, weekly, or monthly." example:"weekly"`
	AttemptCap        *int32          `json:"attempt_cap,omitempty" minimum:"1" maximum:"2147483647" doc:"Maximum submissions per player per period."`
	Metadata          json.RawMessage `json:"metadata,omitempty" doc:"Display metadata, a JSON object, up to 16 KiB."`
}

// projectLeaderboardSettings fills defaults and checks the metadata. Absent
// metadata and JSON null both mean no metadata.
func projectLeaderboardSettings(f ProjectLeaderboardFields, operator string) (projectadmin.LeaderboardSettings, error) {
	s := projectadmin.LeaderboardSettings{
		Name: f.Name, SortOrder: f.SortOrder, ScoreOperator: operator, ResetSchedule: f.ResetSchedule,
		ClientSubmissions: f.ClientSubmissions, ScoreMin: f.ScoreMin, ScoreMax: f.ScoreMax, AttemptCap: f.AttemptCap,
	}
	metadata := f.Metadata
	if s.SortOrder == "" {
		s.SortOrder = "desc"
	}
	if s.ScoreOperator == "" {
		s.ScoreOperator = "best"
	}
	if s.ResetSchedule == "" {
		s.ResetSchedule = "none"
	}
	if len(metadata) == 0 || string(metadata) == "null" {
		return s, nil
	}
	blob, err := projectadmin.NormalizeJSONObject(string(metadata), projectadmin.LeaderboardMetadataMaxBytes)
	if err != nil {
		return s, huma.Error422UnprocessableEntity("metadata: " + err.Error())
	}
	s.Metadata = blob
	return s, nil
}

type projectCreateLeaderboardBody struct {
	ProjectLeaderboardFields
	ScoreOperator string `json:"score_operator,omitempty" doc:"best (default), set, or incr; fixed after creation." example:"best"`
}

type projectCreateLeaderboardInput struct {
	Body projectCreateLeaderboardBody
}

type projectCreatedLeaderboard struct {
	LeaderboardID int64 `json:"leaderboard_id" example:"7"`
	Revision      int64 `json:"revision" example:"1"`
}

func createLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectCreateLeaderboardInput) (projectCreatedLeaderboard, error) {
	b := in.Body
	s, err := projectLeaderboardSettings(b.ProjectLeaderboardFields, b.ScoreOperator)
	if err != nil {
		return projectCreatedLeaderboard{}, err
	}
	if err := projectadmin.ValidateLeaderboard(s, true); err != nil {
		return projectCreatedLeaderboard{}, projectWriteError(err)
	}
	id, err := projectadmin.CreateLeaderboard(ctx, d.Pool, p.TenantID, p.ProjectID, s, projectActor(p))
	if err != nil {
		return projectCreatedLeaderboard{}, projectWriteError(err)
	}
	return projectCreatedLeaderboard{LeaderboardID: id, Revision: 1}, nil
}

type projectUpdateLeaderboardBody struct {
	ExpectedRevision int64 `json:"expected_revision" doc:"The revision number from the leaderboard read." example:"1"`
	ProjectLeaderboardFields
}

type projectUpdateLeaderboardInput struct {
	LeaderboardID int64 `path:"leaderboard_id" doc:"Leaderboard ID." example:"7"`
	Body          projectUpdateLeaderboardBody
}

func updateLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectUpdateLeaderboardInput) (projectRevisionResult, error) {
	b := in.Body
	s, err := projectLeaderboardSettings(b.ProjectLeaderboardFields, "")
	if err != nil {
		return projectRevisionResult{}, err
	}
	if err := projectadmin.ValidateLeaderboard(s, false); err != nil {
		return projectRevisionResult{}, projectWriteError(err)
	}
	rev, err := projectadmin.UpdateLeaderboard(ctx, d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, s, &b.ExpectedRevision, projectActor(p))
	if err != nil {
		return projectRevisionResult{}, projectWriteError(err)
	}
	return projectRevisionResult{Revision: rev}, nil
}

type projectRollbackLeaderboardInput struct {
	LeaderboardID int64 `path:"leaderboard_id" doc:"Leaderboard ID." example:"7"`
	Body          projectRollbackBody
}

type projectLeaderboardRollback struct {
	Revision int64  `json:"revision" example:"4"`
	Note     string `json:"note"`
}

func rollbackLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectRollbackLeaderboardInput) (projectLeaderboardRollback, error) {
	rev, err := projectadmin.RollbackLeaderboard(ctx, d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, in.Body.Revision, &in.Body.ExpectedRevision, projectActor(p))
	if err != nil {
		return projectLeaderboardRollback{}, projectWriteError(err)
	}
	return projectLeaderboardRollback{Revision: rev, Note: "Settings were rolled back. Scores did not change."}, nil
}

type projectDeletedResult struct {
	Deleted bool   `json:"deleted"`
	Note    string `json:"note"`
}

func deleteLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectLeaderboardPath) (projectDeletedResult, error) {
	if err := projectadmin.DeleteLeaderboard(ctx, d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, projectActor(p)); err != nil {
		return projectDeletedResult{}, projectWriteError(err)
	}
	return projectDeletedResult{Deleted: true, Note: "Scores are kept. Use restore_leaderboard to undo."}, nil
}

type projectRestoredResult struct {
	Restored bool `json:"restored"`
}

func restoreLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectLeaderboardPath) (projectRestoredResult, error) {
	if err := projectadmin.RestoreLeaderboard(ctx, d.Pool, p.TenantID, p.ProjectID, in.LeaderboardID, projectActor(p)); err != nil {
		return projectRestoredResult{}, projectWriteError(err)
	}
	return projectRestoredResult{Restored: true}, nil
}

type projectCreateAPIKeyBody struct {
	Label  string   `json:"label" doc:"1 to 64 characters." example:"web build"`
	Scopes []string `json:"scopes,omitempty" doc:"Extra feature scopes: fleet or p2p_relay. matchmaker is always added."`
}

type projectCreateAPIKeyInput struct {
	Body projectCreateAPIKeyBody
}

type projectCreatedAPIKey struct {
	APIKeyID int64          `json:"api_key_id" example:"12"`
	APIKey   string         `json:"api_key" doc:"The key value. It is returned only in this response." example:"ggp_..."`
	KeyType  tenant.KeyType `json:"key_type" example:"publishable"`
	Label    string         `json:"label" example:"web build (management token: ci)"`
	Note     string         `json:"note"`
}

// checkProjectAPIKeyLabel answers 422 unless label is 1 to 64 valid
// characters with no control character.
func checkProjectAPIKeyLabel(label string) error {
	if label == "" || !utf8.ValidString(label) || utf8.RuneCountInString(label) > projectAPIKeyLabelMax {
		return huma.Error422UnprocessableEntity(fmt.Sprintf("label must be 1 to %d characters", projectAPIKeyLabelMax))
	}
	if strings.ContainsFunc(label, unicode.IsControl) {
		return huma.Error422UnprocessableEntity("label must not contain control characters")
	}
	return nil
}

// Notes of the create result, by key type.
const (
	publishableKeyNote = "A publishable key is made to ship in a game client. The value is shown only now. " +
		"Revoke it in the dashboard if it is not needed."
	secretKeyNote = "A secret key is for game servers and back-end tools only. Never ship it in a game client. " +
		"The value is shown only now: store it in a secret store. Revoke it at once if it leaks."
)

func createAPIKey(ctx context.Context, d Deps, p managementtoken.Principal, in *projectCreateAPIKeyInput) (projectCreatedAPIKey, error) {
	return createProjectAPIKey(ctx, d, p, in.Body, tenant.KeyTypePublishable, publishableKeyNote)
}

func createSecretAPIKey(ctx context.Context, d Deps, p managementtoken.Principal, in *projectCreateAPIKeyInput) (projectCreatedAPIKey, error) {
	return createProjectAPIKey(ctx, d, p, in.Body, tenant.KeyTypeSecret, secretKeyNote)
}

// createProjectAPIKey makes a key of keyType for the token's project, with
// the label rules, the feature rules, and the key limit of every token key
// create.
func createProjectAPIKey(ctx context.Context, d Deps, p managementtoken.Principal, body projectCreateAPIKeyBody, keyType tenant.KeyType, note string) (projectCreatedAPIKey, error) {
	label := body.Label
	if err := checkProjectAPIKeyLabel(label); err != nil {
		return projectCreatedAPIKey{}, err
	}
	// The token label goes into the key label, so a person can find the
	// key in the dashboard and revoke it.
	label = fmt.Sprintf("%s (management token: %s)", label, p.Label)
	projectID := p.ProjectID
	id, value, err := projectadmin.CreateAPIKey(ctx, d.Pool, d.RBAC, p.TenantID, projectadmin.NewAPIKey{
		ProjectID: &projectID,
		Label:     label,
		Type:      keyType,
		Scopes:    body.Scopes,
		MaxActive: d.ManagementMaxProjectAPIKeys,
		Switches:  projectadmin.KeySwitches{FleetEnabled: d.ControlPanel.FleetEnabled, RelayEnabled: d.ControlPanel.RelayEnabled},
	}, projectActor(p))
	if err != nil {
		return projectCreatedAPIKey{}, projectWriteError(err)
	}
	return projectCreatedAPIKey{APIKeyID: id, APIKey: value, KeyType: keyType, Label: label, Note: note}, nil
}

type projectSetAllowedOriginsBody struct {
	Origins []string `json:"origins" doc:"The full list of browser origins, for example https://html-classic.itch.zone or http://localhost:5173."`
}

type projectSetAllowedOriginsInput struct {
	Body projectSetAllowedOriginsBody
}

type projectAllowedOrigins struct {
	Origins    []string `json:"origins"`
	OldOrigins []string `json:"old_origins"`
	Note       string   `json:"note"`
}

func setAllowedOrigins(ctx context.Context, d Deps, p managementtoken.Principal, in *projectSetAllowedOriginsInput) (projectAllowedOrigins, error) {
	old, err := projectadmin.SetAllowedOrigins(ctx, d.Pool, p.TenantID, p.ProjectID, in.Body.Origins, d.CORSMaxProjectOrigins, projectActor(p))
	if err != nil {
		return projectAllowedOrigins{}, projectWriteError(err)
	}
	now, err := projectadmin.AllowedOrigins(ctx, d.Pool, p.TenantID, p.ProjectID)
	if err != nil {
		return projectAllowedOrigins{}, err
	}
	return projectAllowedOrigins{
		Origins:    now,
		OldOrigins: old,
		Note: "To undo, call set_allowed_origins with old_origins. Each server uses the new list within " +
			projectadmin.OriginCacheTTL.String() + ".",
	}, nil
}

// projectKeyScope limits a key change to live keys of the token's project.
func projectKeyScope(p managementtoken.Principal) projectadmin.KeyScope {
	return projectadmin.KeyScope{ProjectID: &p.ProjectID, LiveOnly: true}
}

// ProjectAPIKeyPath is exported because huma reads an embedded struct only when its type is exported.
type ProjectAPIKeyPath struct {
	APIKeyID int64 `path:"api_key_id" doc:"API key ID." example:"12"`
}

type projectUpdateAPIKeyLabelInput struct {
	ProjectAPIKeyPath
	Body struct {
		Label string `json:"label" doc:"1 to 64 characters." example:"web build"`
	}
}

type projectAPIKeyUpdated struct {
	Updated bool `json:"updated"`
}

func updateAPIKeyLabel(ctx context.Context, d Deps, p managementtoken.Principal, in *projectUpdateAPIKeyLabelInput) (projectAPIKeyUpdated, error) {
	if err := checkProjectAPIKeyLabel(in.Body.Label); err != nil {
		return projectAPIKeyUpdated{}, err
	}
	if err := projectadmin.UpdateAPIKeyLabel(ctx, d.Pool, d.RBAC, p.TenantID, projectKeyScope(p), in.APIKeyID, in.Body.Label, projectActor(p)); err != nil {
		return projectAPIKeyUpdated{}, projectWriteError(err)
	}
	return projectAPIKeyUpdated{Updated: true}, nil
}

type projectUpdateAPIKeyFeaturesInput struct {
	ProjectAPIKeyPath
	Body struct {
		Scopes []string `json:"scopes" doc:"The full list of feature scopes: matchmaker, fleet, p2p_relay. Scopes not listed are removed." example:"[\"matchmaker\"]"`
	}
}

func updateAPIKeyFeatures(ctx context.Context, d Deps, p managementtoken.Principal, in *projectUpdateAPIKeyFeaturesInput) (projectAPIKeyUpdated, error) {
	sw := projectadmin.KeySwitches{FleetEnabled: d.ControlPanel.FleetEnabled, RelayEnabled: d.ControlPanel.RelayEnabled}
	if err := projectadmin.UpdateAPIKeyFeatures(ctx, d.Pool, d.RBAC, sw, p.TenantID, projectKeyScope(p), in.APIKeyID, in.Body.Scopes, projectActor(p)); err != nil {
		return projectAPIKeyUpdated{}, projectWriteError(err)
	}
	if d.Cache != nil {
		_ = d.Cache.Delete(ctx, ratelimit.APIKeyBucketKey(in.APIKeyID))
	}
	return projectAPIKeyUpdated{Updated: true}, nil
}

type projectAPIKeyRevoked struct {
	Revoked bool `json:"revoked"`
}

func revokeAPIKey(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectAPIKeyPath) (projectAPIKeyRevoked, error) {
	if err := projectadmin.RevokeAPIKey(ctx, d.Pool, d.RBAC, d.Cache, p.TenantID, projectKeyScope(p), in.APIKeyID, projectActor(p)); err != nil {
		return projectAPIKeyRevoked{}, projectWriteError(err)
	}
	return projectAPIKeyRevoked{Revoked: true}, nil
}

type projectSetPlayerDisabledInput struct {
	ProjectPlayerPath
	Body struct {
		Disabled bool `json:"disabled" doc:"true disables the player in this Game Project; false enables it again."`
	}
}

// playerWriteResult reads the player again after a write. A write error is
// mapped first, so a player of another project is 404 before any change.
func playerWriteResult(ctx context.Context, d Deps, p managementtoken.Principal, playerID int64, err error) (projectPlayer, error) {
	if err != nil {
		return projectPlayer{}, projectWriteError(err)
	}
	return readPlayer(ctx, d, p, playerID)
}

func setPlayerDisabled(ctx context.Context, d Deps, p managementtoken.Principal, in *projectSetPlayerDisabledInput) (projectPlayer, error) {
	err := projectadmin.SetPlayerDisabled(ctx, d.Pool, p.TenantID, p.ProjectID, in.PlayerID, in.Body.Disabled, time.Now(), projectActor(p))
	return playerWriteResult(ctx, d, p, in.PlayerID, err)
}

func requestProjectPlayerDelete(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectPlayerPath) (projectPlayer, error) {
	_, err := projectadmin.RequestPlayerDelete(ctx, d.Pool, p.TenantID, p.ProjectID, in.PlayerID,
		deleteGrace(d), projectActor(p))
	return playerWriteResult(ctx, d, p, in.PlayerID, err)
}

func cancelProjectPlayerDelete(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectPlayerPath) (projectPlayer, error) {
	err := projectadmin.CancelPlayerDelete(ctx, d.Pool, p.TenantID, p.ProjectID, in.PlayerID, projectActor(p))
	return playerWriteResult(ctx, d, p, in.PlayerID, err)
}
