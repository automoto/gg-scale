package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/matchmaker/query"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

// projectMaxListRows caps the key list in the health check. The list
// operations are paged instead.
const projectMaxListRows = 50

// ProjectPageInput is the paging input of a list operation, as for
// GET /v1/leaderboards. The cursor is the sort key of the last row of the
// previous page. Its form depends on the list, so a client sends back the
// next_cursor value unchanged.
type ProjectPageInput struct {
	Limit  string `query:"limit" doc:"Rows per page, at most 100. The default is 100." example:"100"`
	Cursor string `query:"cursor" doc:"The next_cursor of the previous page, sent back unchanged. Leave it out for the first page."`
}

func (in ProjectPageInput) limit() int32 {
	return parseLimit(in.Limit, leaderboardListPageSize, leaderboardListPageSize)
}

// trimPage cuts rows, read with one row more than limit, to one page. more is
// true when a next page exists.
func trimPage[T any](rows []T, limit int32) (page []T, more bool) {
	if len(rows) <= int(limit) {
		return rows, false
	}
	return rows[:limit], true
}

// projectQ runs fn in a transaction scoped to the token's tenant. It uses the
// primary, not the read replica: a stale revision number from a replica
// would make the next write fail with 409.
func projectQ(ctx context.Context, d Deps, p managementtoken.Principal, fn func(ctx context.Context, q *sqlcgen.Queries) error) error {
	ctx = db.WithTenant(ctx, p.TenantID)
	return d.Pool.Q(ctx, func(tx pgx.Tx) error { return fn(ctx, sqlcgen.New(tx)) })
}

// clientText returns s only when it is a short identifier: 64 bytes or less
// and only ASCII letters, digits, "_", "-", and ".". Such a value has no
// spaces, so it cannot hold a sentence. Other values become a marker with
// their length, so a player cannot write instructions to an agent.
func clientText(s string) string {
	if len(s) <= 64 && isIdentifier(s) {
		return s
	}
	return fmt.Sprintf("[client text hidden, %d bytes]", len(s))
}

func isIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && c != '-' && c != '.' &&
			(c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

type projectRemoteConfig struct {
	Config   json.RawMessage `json:"config" doc:"The remote config, a JSON object."`
	Revision int64           `json:"revision" doc:"Newest revision number, 0 if the config has no revision." example:"3"`
}

func getRemoteConfig(ctx context.Context, d Deps, p managementtoken.Principal, _ *struct{}) (projectRemoteConfig, error) {
	var out projectRemoteConfig
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		if out.Config, err = q.GetRemoteConfig(ctx, p.ProjectID); err != nil {
			return err
		}
		out.Revision, err = q.LatestSettingsRevision(ctx, sqlcgen.LatestSettingsRevisionParams{
			ProjectID: p.ProjectID, ResourceKind: projectadmin.KindRemoteConfig, ResourceID: p.ProjectID,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errProjectNotFound()
	}
	return out, err
}

type projectLeaderboard struct {
	ID                int64           `json:"id" example:"7"`
	Name              string          `json:"name" example:"weekly-high-scores"`
	SortOrder         string          `json:"sort_order" example:"desc"`
	ScoreOperator     string          `json:"score_operator" example:"best"`
	ClientSubmissions bool            `json:"client_submissions"`
	ScoreMin          *int64          `json:"score_min"`
	ScoreMax          *int64          `json:"score_max"`
	ResetSchedule     string          `json:"reset_schedule" example:"weekly"`
	AttemptCap        *int32          `json:"attempt_cap"`
	Metadata          json.RawMessage `json:"metadata,omitempty" doc:"Display metadata, a JSON object. Absent when the leaderboard has none."`
	CurrentPeriod     int32           `json:"current_period"`
	NextResetAt       *time.Time      `json:"next_reset_at"`
	CreatedAt         *time.Time      `json:"created_at"`
	Revision          *int64          `json:"revision,omitempty" doc:"Newest revision number. Only in the response of a single leaderboard."`
}

type projectDeletedLeaderboard struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	DeletedAt time.Time `json:"deleted_at"`
}

type projectLeaderboardList struct {
	Leaderboards []projectLeaderboard `json:"leaderboards"`
	NextCursor   string               `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
	// A pointer, so the key is absent unless include_deleted is set.
	DeletedLeaderboards *[]projectDeletedLeaderboard `json:"deleted_leaderboards,omitempty" doc:"Deleted leaderboards that can be restored. Only when include_deleted is set."`
}

type projectListLeaderboardsInput struct {
	ProjectPageInput
	IncludeDeleted bool `query:"include_deleted" doc:"Also list deleted leaderboards that can be restored. They are in the first page only."`
}

func listLeaderboards(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListLeaderboardsInput) (projectLeaderboardList, error) {
	limit := in.limit()
	var rows []sqlcgen.ListLeaderboardsPageForProjectRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListLeaderboardsPageForProject(ctx, sqlcgen.ListLeaderboardsPageForProjectParams{
			ProjectID: p.ProjectID, AfterName: in.Cursor, RowLimit: limit + 1,
		})
		return err
	})
	if err != nil {
		return projectLeaderboardList{}, err
	}
	rows, more := trimPage(rows, limit)
	out := projectLeaderboardList{Leaderboards: make([]projectLeaderboard, 0, len(rows))}
	if more {
		out.NextCursor = rows[len(rows)-1].Name
	}
	for _, r := range rows {
		out.Leaderboards = append(out.Leaderboards, projectLeaderboard{
			ID: r.ID, Name: r.Name, SortOrder: r.SortOrder, ScoreOperator: r.ScoreOperator,
			ClientSubmissions: r.ClientSubmissions, ScoreMin: r.ScoreMin, ScoreMax: r.ScoreMax,
			ResetSchedule: r.ResetSchedule, AttemptCap: r.AttemptCap, Metadata: r.Metadata,
			CurrentPeriod: r.CurrentPeriod, NextResetAt: timestamptzPtr(r.NextResetAt), CreatedAt: timestamptzPtr(r.CreatedAt),
		})
	}
	if !in.IncludeDeleted || in.Cursor != "" {
		return out, nil
	}
	deleted, err := projectadmin.ListDeletedLeaderboards(ctx, d.Pool, p.TenantID, p.ProjectID)
	if err != nil {
		return projectLeaderboardList{}, err
	}
	views := make([]projectDeletedLeaderboard, 0, len(deleted))
	for _, b := range deleted {
		views = append(views, projectDeletedLeaderboard{ID: b.ID, Name: b.Name, DeletedAt: b.DeletedAt})
	}
	out.DeletedLeaderboards = &views
	return out, nil
}

type projectLeaderboardPath struct {
	LeaderboardID int64 `path:"leaderboard_id" doc:"Leaderboard ID." example:"7"`
}

func getLeaderboard(ctx context.Context, d Deps, p managementtoken.Principal, in *projectLeaderboardPath) (projectLeaderboard, error) {
	var out projectLeaderboard
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		r, err := q.GetLeaderboardForControlPanel(ctx, sqlcgen.GetLeaderboardForControlPanelParams{
			ProjectID: p.ProjectID, ID: in.LeaderboardID,
		})
		if err != nil {
			return err
		}
		rev, err := q.LatestSettingsRevision(ctx, sqlcgen.LatestSettingsRevisionParams{
			ProjectID: p.ProjectID, ResourceKind: projectadmin.KindLeaderboard, ResourceID: r.ID,
		})
		out = projectLeaderboard{
			ID: r.ID, Name: r.Name, SortOrder: r.SortOrder, ScoreOperator: r.ScoreOperator,
			ClientSubmissions: r.ClientSubmissions, ScoreMin: r.ScoreMin, ScoreMax: r.ScoreMax,
			ResetSchedule: r.ResetSchedule, AttemptCap: r.AttemptCap, Metadata: r.Metadata,
			CurrentPeriod: r.CurrentPeriod, NextResetAt: timestamptzPtr(r.NextResetAt), CreatedAt: timestamptzPtr(r.CreatedAt),
			Revision: &rev,
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return projectLeaderboard{}, errProjectNotFound()
	}
	return out, err
}

type projectAPIKey struct {
	ID          int64      `json:"id" example:"12"`
	Label       string     `json:"label" example:"web build"`
	Type        string     `json:"type" example:"publishable"`
	Scopes      []string   `json:"scopes"`
	AllProjects bool       `json:"all_projects" doc:"True for a tenant-wide key that works for each Game Project."`
	CreatedAt   *time.Time `json:"created_at"`
}

// projectActiveKeys returns the unrevoked keys that work for the token's
// project: keys bound to it and tenant-wide keys (project_id NULL).
func projectActiveKeys(ctx context.Context, d Deps, p managementtoken.Principal) ([]projectAPIKey, error) {
	var rows []sqlcgen.ListAPIKeysRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListAPIKeys(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := []projectAPIKey{}
	for _, r := range rows {
		if r.RevokedAt.Valid || (r.ProjectID != nil && *r.ProjectID != p.ProjectID) {
			continue
		}
		v := projectAPIKey{ID: r.ID, Type: r.KeyType, Scopes: r.Scopes, AllProjects: r.ProjectID == nil, CreatedAt: timestamptzPtr(r.CreatedAt)}
		if r.Label != nil {
			v.Label = *r.Label
		}
		out = append(out, v)
	}
	return out, nil
}

type projectAPIKeyList struct {
	APIKeys    []projectAPIKey `json:"api_keys"`
	NextCursor string          `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
}

// listAPIKeys pages by key ID, newest first: the cursor is the ID of the
// last key of the page.
func listAPIKeys(ctx context.Context, d Deps, p managementtoken.Principal, in *ProjectPageInput) (projectAPIKeyList, error) {
	limit := in.limit()
	var rows []sqlcgen.ListActiveAPIKeysPageForProjectRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListActiveAPIKeysPageForProject(ctx, sqlcgen.ListActiveAPIKeysPageForProjectParams{
			ProjectID: &p.ProjectID, BeforeID: parseCursor(in.Cursor), RowLimit: limit + 1,
		})
		return err
	})
	if err != nil {
		return projectAPIKeyList{}, err
	}
	rows, more := trimPage(rows, limit)
	out := projectAPIKeyList{APIKeys: make([]projectAPIKey, 0, len(rows))}
	if more {
		out.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	for _, r := range rows {
		k := projectAPIKey{ID: r.ID, Type: r.KeyType, Scopes: r.Scopes, AllProjects: r.ProjectID == nil, CreatedAt: timestamptzPtr(r.CreatedAt)}
		if r.Label != nil {
			k.Label = *r.Label
		}
		out.APIKeys = append(out.APIKeys, k)
	}
	return out, nil
}

// projectPlayer is a player of the token's project. email and display_name
// are pointers, so each key is absent unless the token has players:pii. Both
// are text that players wrote.
type projectPlayer struct {
	ID                int64      `json:"id" example:"42"`
	ExternalID        string     `json:"external_id" doc:"Set by the sign-in of the game. It may hold text that the game chose." example:"7a1c0b52-5d3e-4a35-9a54-6f1f63f2f0a1"`
	EmailVerified     bool       `json:"email_verified"`
	Disabled          bool       `json:"disabled"`
	DeleteRequested   bool       `json:"delete_requested" doc:"A delete request is pending and the player is in the grace period."`
	CreatedAt         *time.Time `json:"created_at"`
	DisabledAt        *time.Time `json:"disabled_at"`
	DeleteRequestedAt *time.Time `json:"delete_requested_at"`
	Email             *string    `json:"email,omitempty" doc:"Text that the player wrote. Only with the players:pii scope; the empty string means the player has no email."`
	DisplayName       *string    `json:"display_name,omitempty" doc:"Text that the player wrote. Only with the players:pii scope; the empty string means the player has no display name."`
}

// playerView builds the result for one row. It leaves email and display name
// out unless pii is true.
func playerView(pii bool, id int64, externalID, email, displayName string, verifiedAt, disabledAt, deleteAt, createdAt pgtype.Timestamptz) projectPlayer {
	v := projectPlayer{
		ID: id, ExternalID: externalID,
		EmailVerified: verifiedAt.Valid, Disabled: disabledAt.Valid, DeleteRequested: deleteAt.Valid,
		CreatedAt: timestamptzPtr(createdAt), DisabledAt: timestamptzPtr(disabledAt), DeleteRequestedAt: timestamptzPtr(deleteAt),
	}
	if pii {
		v.Email, v.DisplayName = &email, &displayName
	}
	return v
}

type projectListPlayersInput struct {
	ProjectPageInput
	PlayerID   int64  `query:"player_id" doc:"Only the player with this ID."`
	ExternalID string `query:"external_id" maxLength:"255" doc:"Only the player with exactly this external ID."`
}

type projectPlayerList struct {
	Players    []projectPlayer `json:"players"`
	NextCursor string          `json:"next_cursor,omitempty" doc:"Send as cursor for the next page. Absent on the last page."`
}

// listPlayers pages by player ID, newest first: the cursor is the ID of the
// last player of the page. The pair of players:pii is the pair of
// players:read, so the scope test alone decides the personal fields.
func listPlayers(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListPlayersInput) (projectPlayerList, error) {
	limit := in.limit()
	var rows []sqlcgen.ListPlayersPageForProjectRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListPlayersPageForProject(ctx, sqlcgen.ListPlayersPageForProjectParams{
			ProjectID: p.ProjectID, PlayerID: in.PlayerID, ExternalID: in.ExternalID,
			BeforeID: parseCursor(in.Cursor), RowLimit: limit + 1,
		})
		return err
	})
	if err != nil {
		return projectPlayerList{}, err
	}
	rows, more := trimPage(rows, limit)
	out := projectPlayerList{Players: make([]projectPlayer, 0, len(rows))}
	if more {
		out.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	pii := p.HasScope(managementtoken.ScopePlayersPII)
	for _, r := range rows {
		out.Players = append(out.Players, playerView(pii, r.ID, r.ExternalID, r.Email, r.DisplayName,
			r.EmailVerifiedAt, r.DisabledAt, r.DeleteRequestedAt, r.CreatedAt))
	}
	return out, nil
}

type projectPlayerPath struct {
	PlayerID int64 `path:"player_id" doc:"Player ID." example:"42"`
}

func getPlayer(ctx context.Context, d Deps, p managementtoken.Principal, in *projectPlayerPath) (projectPlayer, error) {
	var r sqlcgen.GetPlayerPageRowForProjectRow
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		r, err = q.GetPlayerPageRowForProject(ctx, sqlcgen.GetPlayerPageRowForProjectParams{ProjectID: p.ProjectID, ID: in.PlayerID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return projectPlayer{}, errProjectNotFound()
	}
	if err != nil {
		return projectPlayer{}, err
	}
	return playerView(p.HasScope(managementtoken.ScopePlayersPII), r.ID, r.ExternalID, r.Email, r.DisplayName,
		r.EmailVerifiedAt, r.DisabledAt, r.DeleteRequestedAt, r.CreatedAt), nil
}

type projectListRevisionsInput struct {
	Resource      string `query:"resource" required:"true" enum:"remote_config,leaderboard" doc:"remote_config or leaderboard."`
	LeaderboardID int64  `query:"leaderboard_id" doc:"Required when resource is leaderboard."`
}

type projectRevision struct {
	Revision          int64           `json:"revision" example:"2"`
	Source            string          `json:"source" example:"token" doc:"dashboard, token, rollback, or base."`
	ActorUserID       *int64          `json:"actor_user_id"`
	ManagementTokenID *int64          `json:"management_token_id"`
	CreatedAt         time.Time       `json:"created_at"`
	Snapshot          json.RawMessage `json:"snapshot" doc:"The resource value of this revision."`
}

type projectRevisionList struct {
	Revisions []projectRevision `json:"revisions"`
}

func listRevisions(ctx context.Context, d Deps, p managementtoken.Principal, in *projectListRevisionsInput) (projectRevisionList, error) {
	// The enum of Resource refuses other values before the handler runs.
	kind, resourceID, scope := projectadmin.KindRemoteConfig, p.ProjectID, managementtoken.ScopeConfigRead
	if in.Resource == projectadmin.KindLeaderboard {
		if in.LeaderboardID == 0 {
			return projectRevisionList{}, huma.Error422UnprocessableEntity("leaderboard_id is required when resource is leaderboard")
		}
		kind, resourceID, scope = projectadmin.KindLeaderboard, in.LeaderboardID, managementtoken.ScopeLeaderboardsRead
	}
	if err := requireScope(ctx, d, p, scope); err != nil {
		return projectRevisionList{}, err
	}
	revs, err := projectadmin.ListRevisions(ctx, d.Pool, p.TenantID, p.ProjectID, kind, resourceID)
	if err != nil {
		return projectRevisionList{}, err
	}
	out := projectRevisionList{Revisions: make([]projectRevision, 0, len(revs))}
	for _, r := range revs {
		out.Revisions = append(out.Revisions, projectRevision{
			Revision: r.Revision, Source: r.Source, ActorUserID: r.ActorUserID,
			ManagementTokenID: r.TokenID, CreatedAt: r.CreatedAt, Snapshot: r.Snapshot,
		})
	}
	return out, nil
}

type projectTicketTraceInput struct {
	TicketID int64 `query:"ticket_id" doc:"Matchmaking ticket ID."`
	PlayerID int64 `query:"player_id" doc:"Player ID; finds the player's queued ticket."`
}

type projectTicketTrace struct {
	TicketID          int64              `json:"ticket_id" example:"91"`
	PlayerID          int64              `json:"player_id" example:"42"`
	PartyID           *int64             `json:"party_id"`
	Status            string             `json:"status" example:"queued"`
	FailureReason     *string            `json:"failure_reason"`
	Claimed           bool               `json:"claimed"`
	Mode              string             `json:"mode" example:"match_only"`
	FleetID           *int64             `json:"fleet_id"`
	Region            string             `json:"region"`
	GameMode          string             `json:"game_mode"`
	MinCount          int32              `json:"min_count"`
	MaxCount          int32              `json:"max_count"`
	CountMultiple     int32              `json:"count_multiple"`
	AllowCrossRegion  bool               `json:"allow_cross_region"`
	Query             string             `json:"query"`
	StringProperties  map[string]string  `json:"string_properties"`
	NumericProperties map[string]float64 `json:"numeric_properties"`
	AttributesBytes   int32              `json:"attributes_bytes"`
	CreatedAt         *time.Time         `json:"created_at"`
	MatchedAt         *time.Time         `json:"matched_at"`
	ExpiresAt         *time.Time         `json:"expires_at"`
	// Tickets waiting in the same bucket (mode, fleet, region, game mode),
	// this one included when it is queued.
	QueuedInSameBucket int64  `json:"queued_in_same_bucket"`
	Note               string `json:"note"`
}

func matchmakingTicketTrace(ctx context.Context, d Deps, p managementtoken.Principal, in *projectTicketTraceInput) (projectTicketTrace, error) {
	if (in.TicketID == 0) == (in.PlayerID == 0) {
		return projectTicketTrace{}, huma.Error422UnprocessableEntity("give exactly one of ticket_id or player_id")
	}
	var t sqlcgen.GetMatchmakingTicketForTraceRow
	var queued int64
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		id := in.TicketID
		if id == 0 {
			var err error
			id, err = q.GetQueuedTicketForPlayer(ctx, sqlcgen.GetQueuedTicketForPlayerParams{
				ProjectID: p.ProjectID, PlayerID: in.PlayerID,
			})
			if err != nil {
				return err
			}
		}
		var err error
		t, err = q.GetMatchmakingTicketForTrace(ctx, sqlcgen.GetMatchmakingTicketForTraceParams{ProjectID: p.ProjectID, ID: id})
		if err != nil {
			return err
		}
		queued, err = q.CountQueuedTicketsLike(ctx, sqlcgen.CountQueuedTicketsLikeParams{
			ProjectID: p.ProjectID, Mode: t.Mode, FleetID: t.FleetID, Region: t.Region, GameMode: t.GameMode,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) && in.PlayerID != 0 {
		return projectTicketTrace{}, huma.Error404NotFound("this player has no ticket in the queue now; give the ticket_id to see a finished ticket")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return projectTicketTrace{}, errProjectNotFound()
	}
	if err != nil {
		return projectTicketTrace{}, err
	}
	return ticketTraceView(t, queued), nil
}

// ticketTraceView builds the trace result. Each string the game client
// supplied goes through clientText; the attributes blob is reported by size
// only.
func ticketTraceView(t sqlcgen.GetMatchmakingTicketForTraceRow, queued int64) projectTicketTrace {
	strProps := map[string]string{}
	var raw map[string]string
	if json.Unmarshal(t.StringProperties, &raw) == nil {
		for k, v := range raw {
			strProps[clientText(k)] = clientText(v)
		}
	}
	numProps := map[string]float64{}
	var rawNum map[string]float64
	if json.Unmarshal(t.NumericProperties, &rawNum) == nil {
		for k, v := range rawNum {
			numProps[clientText(k)] = v
		}
	}
	queryText := "[unparseable query hidden]"
	if expr, err := query.Parse(t.Query); err == nil {
		queryText = query.Format(expr, clientText)
	}
	return projectTicketTrace{
		TicketID:           t.ID,
		PlayerID:           t.PlayerID,
		PartyID:            t.PartyID,
		Status:             t.Status,
		FailureReason:      t.FailureReason,
		Claimed:            t.Claimed,
		Mode:               t.Mode,
		FleetID:            t.FleetID,
		Region:             clientText(t.Region),
		GameMode:           clientText(t.GameMode),
		MinCount:           t.MinCount,
		MaxCount:           t.MaxCount,
		CountMultiple:      t.CountMultiple,
		AllowCrossRegion:   t.AllowCrossRegion,
		Query:              queryText,
		StringProperties:   strProps,
		NumericProperties:  numProps,
		AttributesBytes:    t.AttributesBytes,
		CreatedAt:          timestamptzPtr(t.CreatedAt),
		MatchedAt:          timestamptzPtr(t.MatchedAt),
		ExpiresAt:          timestamptzPtr(t.ExpiresAt),
		QueuedInSameBucket: queued,
		Note:               "Finished tickets are kept for 24 hours.",
	}
}

// quotaState reports a tenant-level quota without its counts.
func quotaState(limit, current int64) string {
	switch {
	case limit == quota.Unlimited:
		return "ok"
	case current >= limit:
		return "at_limit"
	case current*5 >= limit*4:
		return "near_limit"
	}
	return "ok"
}

type projectHealth struct {
	Problems []string            `json:"problems"`
	Checks   projectHealthChecks `json:"checks"`
}

// projectHealthChecks leaves out each section that needs a read scope the
// token does not have. The pointers make the key absent, so a client can
// tell a left-out section from an empty one.
type projectHealthChecks struct {
	APIKeys            *[]projectAPIKey      `json:"api_keys,omitempty" doc:"Absent without the keys:read scope."`
	FeatureGrants      map[string]bool       `json:"feature_grants"`
	ServerSwitches     projectServerSwitches `json:"server_switches"`
	CORSAllowedOrigins *projectHealthOrigins `json:"cors_allowed_origins,omitempty" doc:"Absent without the config:read scope."`
	SteamSignIn        *bool                 `json:"steam_sign_in,omitempty" doc:"Absent without the config:read scope."`
	RemoteConfigBytes  *int                  `json:"remote_config_bytes,omitempty" doc:"Absent without the config:read scope."`
	Quotas             projectHealthQuotas   `json:"quotas"`
}

type projectServerSwitches struct {
	Fleet           bool `json:"fleet"`
	P2PRelay        bool `json:"p2p_relay"`
	RelayConfigured bool `json:"relay_configured"`
}

type projectHealthOrigins struct {
	Server  []string `json:"server"`
	Project []string `json:"project"`
	Note    string   `json:"note"`
}

// projectHealthQuotas reports tenant-level quotas as a state only (ok,
// near_limit, at_limit), and only when quotas are enforced.
type projectHealthQuotas struct {
	Enforced bool   `json:"enforced"`
	Projects string `json:"projects,omitempty" enum:"ok,near_limit,at_limit"`
	Players  string `json:"players,omitempty" enum:"ok,near_limit,at_limit"`
	// open_sessions is a project quota, so its counts are returned.
	OpenSessions projectQuotaCount `json:"open_sessions"`
	// leaderboards is a project limit that applies also when quotas are
	// not enforced.
	Leaderboards projectQuotaCount `json:"leaderboards"`
}

type projectQuotaCount struct {
	Current int64 `json:"current"`
	Limit   int64 `json:"limit"`
}

// projectHealthCheck is open to every token. The pairs of keys:read and
// config:read are (project, manage), which the token middleware checks, so
// the scope test alone decides each section.
func projectHealthCheck(ctx context.Context, d Deps, p managementtoken.Principal, _ *struct{}) (projectHealth, error) {
	fleetEnabled, relayEnabled, relayConfigured := d.ControlPanel.FleetEnabled, d.ControlPanel.RelayEnabled, d.RelayIssuer != nil
	showKeys, showConfig := p.HasScope(managementtoken.ScopeKeysRead), p.HasScope(managementtoken.ScopeConfigRead)
	problems := []string{}
	var checks projectHealthChecks
	keyScopes := map[string]bool{}
	if showKeys {
		keys, err := projectActiveKeys(ctx, d, p)
		if err != nil {
			return projectHealth{}, err
		}
		var hasPublishable bool
		for _, k := range keys {
			if k.Type == string(tenant.KeyTypePublishable) {
				hasPublishable = true
			}
			for _, s := range k.Scopes {
				keyScopes[s] = true
			}
		}
		if !hasPublishable {
			problems = append(problems, "No active publishable API key works for this Game Project. Game clients cannot call the API.")
		}
		keys = keys[:min(len(keys), projectMaxListRows)]
		checks.APIKeys = &keys
	}

	grants := map[string]bool{}
	for _, f := range []rbac.Feature{rbac.FeatureMatchmaker, rbac.FeatureP2PRelay, rbac.FeatureDedicatedServers} {
		on, err := d.RBAC.FeatureEnabled(ctx, p.TenantID, p.ProjectID, f)
		if err != nil {
			return projectHealth{}, err
		}
		grants[string(f)] = on
	}
	if showKeys && grants[string(rbac.FeatureMatchmaker)] && !keyScopes[tenant.ScopeMatchmaker] {
		problems = append(problems, "Matchmaking is granted, but no active API key has the matchmaker scope. /v1/matchmaker calls get 403.")
	}
	if grants[string(rbac.FeatureP2PRelay)] {
		switch {
		case !relayEnabled || !relayConfigured:
			problems = append(problems, "The relay is granted, but the server relay is off or not configured. /v1/relay is not available.")
		case showKeys && !keyScopes[tenant.ScopeP2PRelay]:
			problems = append(problems, "The relay is granted, but no active API key has the p2p_relay scope.")
		}
	}
	if grants[string(rbac.FeatureDedicatedServers)] && !fleetEnabled {
		problems = append(problems, "Dedicated servers are granted, but FEATURE_FLEET_ENABLED is off on the server.")
	}

	var (
		qc           sqlcgen.GetTenantQuotaContextRow
		projects     int64
		openSessions int64
		boards       int64
		configBytes  int
		steam        sqlcgen.GetProjectSteamAuthConfigForControlPanelRow
	)
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		if qc, err = q.GetTenantQuotaContext(ctx); err != nil {
			return err
		}
		if projects, err = q.CountProjectsForTenant(ctx); err != nil {
			return err
		}
		if openSessions, err = q.CountOpenGameSessionsForProject(ctx, p.ProjectID); err != nil {
			return err
		}
		if boards, err = q.CountLiveLeaderboardsForProject(ctx, p.ProjectID); err != nil {
			return err
		}
		if !showConfig {
			return nil
		}
		cfg, err := q.GetRemoteConfig(ctx, p.ProjectID)
		if err != nil {
			return err
		}
		configBytes = len(cfg)
		steam, err = q.GetProjectSteamAuthConfigForControlPanel(ctx, sqlcgen.GetProjectSteamAuthConfigForControlPanelParams{
			ProjectID: p.ProjectID, TenantID: p.TenantID,
		})
		return err
	})
	if err != nil {
		return projectHealth{}, err
	}
	limits, err := quota.ResolveSnapshot(int(qc.Tier), qc.Overrides)
	if err != nil {
		return projectHealth{}, err
	}
	quotas := projectHealthQuotas{
		Enforced:     qc.EnforceQuotas,
		OpenSessions: projectQuotaCount{Current: openSessions, Limit: limits.OpenSessionsPerProject},
		Leaderboards: projectQuotaCount{Current: boards, Limit: limits.LeaderboardsPerProject},
	}
	if quotaState(limits.LeaderboardsPerProject, boards) == "at_limit" {
		problems = append(problems, fmt.Sprintf("The Game Project is at its leaderboard limit (%d). New leaderboards are refused.", limits.LeaderboardsPerProject))
	}
	if qc.EnforceQuotas {
		quotas.Projects = quotaState(int64(limits.Projects), projects)
		quotas.Players = quotaState(limits.Players, qc.PlayerCount)
		for axis, state := range map[string]string{quota.AxisProjects: quotas.Projects, quota.AxisPlayers: quotas.Players} {
			if state == "at_limit" {
				problems = append(problems, fmt.Sprintf("The Account Tenant is at its %s quota. New %s are refused.", axis, axis))
			}
		}
	}

	if showConfig {
		steamConfigured := steam.SteamAppID != "" && steam.SteamKeyConfigured != nil && *steam.SteamKeyConfigured
		if steam.SteamAppID != "" && !steamConfigured {
			problems = append(problems, "Steam sign-in has an App ID but no Web API key, so it is off.")
		}
		serverOrigins := d.CORSAllowedOrigins
		if len(serverOrigins) == 0 {
			serverOrigins = []string{"*"}
		}
		projectOrigins, err := projectadmin.AllowedOrigins(ctx, d.Pool, p.TenantID, p.ProjectID)
		if err != nil {
			return projectHealth{}, err
		}
		checks.CORSAllowedOrigins = &projectHealthOrigins{
			Server:  serverOrigins,
			Project: projectOrigins,
			Note: "A browser origin is accepted if the server list or the list of any Game Project has it. " +
				"A change reaches each server within " + projectadmin.OriginCacheTTL.String() + ".",
		}
		checks.SteamSignIn = &steamConfigured
		checks.RemoteConfigBytes = &configBytes
	}
	sort.Strings(problems)
	checks.FeatureGrants = grants
	checks.ServerSwitches = projectServerSwitches{Fleet: fleetEnabled, P2PRelay: relayEnabled, RelayConfigured: relayConfigured}
	checks.Quotas = quotas
	return projectHealth{Problems: problems, Checks: checks}, nil
}

type projectToken struct {
	TokenID      int64     `json:"token_id" example:"5"`
	Label        string    `json:"label" example:"claude agent"`
	ProjectID    int64     `json:"project_id" example:"3"`
	ProjectName  string    `json:"project_name" example:"My Game"`
	TenantID     int64     `json:"tenant_id" example:"2"`
	Scopes       []string  `json:"scopes" doc:"The scopes of the token."`
	ActiveScopes []string  `json:"active_scopes" doc:"The scopes of the token that the person who created it can still use now."`
	ExpiresAt    time.Time `json:"expires_at"`
}

// activeScopes returns the scopes of the token whose creator pair the creator
// passes now, with the same check as the per-request creator check.
func activeScopes(ctx context.Context, d Deps, p managementtoken.Principal) ([]string, error) {
	out := []string{}
	for _, name := range p.Scopes {
		sp, ok := managementtoken.Scope(name)
		if !ok {
			continue
		}
		allowed, err := d.RBAC.CanControlPanel(p.CreatedByUserID, p.TenantID, sp.Pair(p.ProjectID), sp.Action)
		if err != nil {
			return nil, serverError(ctx, "project management: active scopes", err)
		}
		if allowed {
			out = append(out, name)
		}
	}
	return out, nil
}

// getToken is open to every token.
func getToken(ctx context.Context, d Deps, p managementtoken.Principal, _ *struct{}) (projectToken, error) {
	var name string
	err := projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		name, err = q.GetProjectName(ctx, p.ProjectID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return projectToken{}, errProjectNotFound()
	}
	if err != nil {
		return projectToken{}, err
	}
	active, err := activeScopes(ctx, d, p)
	if err != nil {
		return projectToken{}, err
	}
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return projectToken{
		TokenID: p.TokenID, Label: p.Label, ProjectID: p.ProjectID, ProjectName: name, TenantID: p.TenantID,
		Scopes: scopes, ActiveScopes: active, ExpiresAt: p.ExpiresAt,
	}, nil
}
