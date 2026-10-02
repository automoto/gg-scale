package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/matchmaker/query"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

// maxListRows caps each list result. There is no paging in this release.
const maxListRows = 50

// q runs fn in a transaction scoped to the token's tenant.
func (h *handler) q(ctx context.Context, p Principal, fn func(ctx context.Context, q *sqlcgen.Queries) error) error {
	ctx = db.WithTenant(ctx, p.TenantID)
	return h.d.Pool.Q(ctx, func(tx pgx.Tx) error { return fn(ctx, sqlcgen.New(tx)) })
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// clientText returns s only when it is a short identifier: 64 bytes or less
// and only ASCII letters, digits, "_", "-", and ".". Such a value has no
// spaces, so it cannot hold a sentence. Other values become a marker with
// their length, so a player cannot write instructions to the agent.
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

func getRemoteConfig(ctx context.Context, h *handler, p Principal, _ noInput) (any, error) {
	var out struct {
		Config   json.RawMessage `json:"config"`
		Revision int64           `json:"revision"`
	}
	err := h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
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
		return nil, errNotFound
	}
	return out, err
}

type leaderboardView struct {
	ID                int64           `json:"id"`
	Name              string          `json:"name"`
	SortOrder         string          `json:"sort_order"`
	ScoreOperator     string          `json:"score_operator"`
	ClientSubmissions bool            `json:"client_submissions"`
	ScoreMin          *int64          `json:"score_min"`
	ScoreMax          *int64          `json:"score_max"`
	ResetSchedule     string          `json:"reset_schedule"`
	AttemptCap        *int32          `json:"attempt_cap"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	CurrentPeriod     int32           `json:"current_period"`
	NextResetAt       *time.Time      `json:"next_reset_at"`
	CreatedAt         *time.Time      `json:"created_at"`
	Revision          *int64          `json:"revision,omitempty"`
}

func listLeaderboards(ctx context.Context, h *handler, p Principal, in listLeaderboardsArgs) (any, error) {
	var rows []sqlcgen.ListLeaderboardsForProjectRow
	err := h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListLeaderboardsForProject(ctx, p.ProjectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"truncated": len(rows) > maxListRows}
	boards := make([]leaderboardView, 0, min(len(rows), maxListRows))
	for _, r := range rows[:min(len(rows), maxListRows)] {
		boards = append(boards, leaderboardView{
			ID: r.ID, Name: r.Name, SortOrder: r.SortOrder, ScoreOperator: r.ScoreOperator,
			ClientSubmissions: r.ClientSubmissions, ScoreMin: r.ScoreMin, ScoreMax: r.ScoreMax,
			ResetSchedule: r.ResetSchedule, AttemptCap: r.AttemptCap, Metadata: r.Metadata,
			CurrentPeriod: r.CurrentPeriod, NextResetAt: timePtr(r.NextResetAt), CreatedAt: timePtr(r.CreatedAt),
		})
	}
	out["leaderboards"] = boards
	if in.IncludeDeleted {
		deleted, err := projectadmin.ListDeletedLeaderboards(ctx, h.d.Pool, p.TenantID, p.ProjectID)
		if err != nil {
			return nil, err
		}
		out["deleted_leaderboards"] = deleted
	}
	return out, nil
}

func getLeaderboard(ctx context.Context, h *handler, p Principal, in leaderboardArgs) (any, error) {
	var out leaderboardView
	err := h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		r, err := q.GetLeaderboardForControlPanel(ctx, sqlcgen.GetLeaderboardForControlPanelParams{
			ProjectID: p.ProjectID, ID: in.LeaderboardID,
		})
		if err != nil {
			return err
		}
		rev, err := q.LatestSettingsRevision(ctx, sqlcgen.LatestSettingsRevisionParams{
			ProjectID: p.ProjectID, ResourceKind: projectadmin.KindLeaderboard, ResourceID: r.ID,
		})
		out = leaderboardView{
			ID: r.ID, Name: r.Name, SortOrder: r.SortOrder, ScoreOperator: r.ScoreOperator,
			ClientSubmissions: r.ClientSubmissions, ScoreMin: r.ScoreMin, ScoreMax: r.ScoreMax,
			ResetSchedule: r.ResetSchedule, AttemptCap: r.AttemptCap, Metadata: r.Metadata,
			CurrentPeriod: r.CurrentPeriod, NextResetAt: timePtr(r.NextResetAt), CreatedAt: timePtr(r.CreatedAt),
			Revision: &rev,
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return out, err
}

type apiKeyView struct {
	ID          int64      `json:"id"`
	Label       string     `json:"label"`
	Type        string     `json:"type"`
	Scopes      []string   `json:"scopes"`
	AllProjects bool       `json:"all_projects"`
	CreatedAt   *time.Time `json:"created_at"`
}

// activeKeys returns the unrevoked keys that work for the token's project: keys
// bound to it and tenant-wide keys (project_id NULL).
func (h *handler) activeKeys(ctx context.Context, p Principal) ([]apiKeyView, error) {
	var rows []sqlcgen.ListAPIKeysRow
	err := h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		rows, err = q.ListAPIKeys(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := []apiKeyView{}
	for _, r := range rows {
		if r.RevokedAt.Valid || (r.ProjectID != nil && *r.ProjectID != p.ProjectID) {
			continue
		}
		v := apiKeyView{ID: r.ID, Type: r.KeyType, Scopes: r.Scopes, AllProjects: r.ProjectID == nil, CreatedAt: timePtr(r.CreatedAt)}
		if r.Label != nil {
			v.Label = *r.Label
		}
		out = append(out, v)
	}
	return out, nil
}

func listAPIKeys(ctx context.Context, h *handler, p Principal, _ noInput) (any, error) {
	keys, err := h.activeKeys(ctx, p)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"api_keys":  keys[:min(len(keys), maxListRows)],
		"truncated": len(keys) > maxListRows,
	}, nil
}

func listRevisions(ctx context.Context, h *handler, p Principal, in listRevisionsArgs) (any, error) {
	var kind string
	var resourceID int64
	switch in.Resource {
	case projectadmin.KindRemoteConfig:
		kind, resourceID = projectadmin.KindRemoteConfig, p.ProjectID
	case projectadmin.KindLeaderboard:
		if in.LeaderboardID == 0 {
			return nil, errorf("leaderboard_id is required when resource is leaderboard")
		}
		kind, resourceID = projectadmin.KindLeaderboard, in.LeaderboardID
	default:
		return nil, errorf("resource must be remote_config or leaderboard")
	}
	revs, err := projectadmin.ListRevisions(ctx, h.d.Pool, p.TenantID, p.ProjectID, kind, resourceID)
	if err != nil {
		return nil, err
	}
	type revView struct {
		Revision    int64           `json:"revision"`
		Source      string          `json:"source"`
		ActorUserID *int64          `json:"actor_user_id"`
		MCPTokenID  *int64          `json:"mcp_token_id"`
		CreatedAt   time.Time       `json:"created_at"`
		Snapshot    json.RawMessage `json:"snapshot"`
	}
	out := make([]revView, 0, len(revs))
	for _, r := range revs {
		out = append(out, revView{r.Revision, r.Source, r.ActorUserID, r.TokenID, r.CreatedAt, r.Snapshot})
	}
	return map[string]any{"revisions": out}, nil
}

func matchmakingTicketTrace(ctx context.Context, h *handler, p Principal, in ticketTraceArgs) (any, error) {
	if (in.TicketID == 0) == (in.PlayerID == 0) {
		return nil, errorf("give exactly one of ticket_id or player_id")
	}
	var t sqlcgen.GetMatchmakingTicketForTraceRow
	var queued int64
	err := h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
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
	if errors.Is(err, pgx.ErrNoRows) {
		if in.PlayerID != 0 {
			return nil, errorf("this player has no ticket in the queue now; give the ticket_id to see a finished ticket")
		}
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	return traceView(t, queued), nil
}

// traceView builds the trace result. Each string the game client supplied
// goes through clientText; the attributes blob is reported by size only.
func traceView(t sqlcgen.GetMatchmakingTicketForTraceRow, queued int64) map[string]any {
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
	return map[string]any{
		"ticket_id":          t.ID,
		"player_id":          t.PlayerID,
		"party_id":           t.PartyID,
		"status":             t.Status,
		"failure_reason":     t.FailureReason,
		"claimed":            t.Claimed,
		"mode":               t.Mode,
		"fleet_id":           t.FleetID,
		"region":             clientText(t.Region),
		"game_mode":          clientText(t.GameMode),
		"min_count":          t.MinCount,
		"max_count":          t.MaxCount,
		"count_multiple":     t.CountMultiple,
		"allow_cross_region": t.AllowCrossRegion,
		"query":              queryText,
		"string_properties":  strProps,
		"numeric_properties": numProps,
		"attributes_bytes":   t.AttributesBytes,
		"created_at":         timePtr(t.CreatedAt),
		"matched_at":         timePtr(t.MatchedAt),
		"expires_at":         timePtr(t.ExpiresAt),
		// Tickets waiting in the same bucket (mode, fleet, region, game mode),
		// this one included when it is queued.
		"queued_in_same_bucket": queued,
		"note":                  "Finished tickets are kept for 24 hours.",
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

func projectHealthCheck(ctx context.Context, h *handler, p Principal, _ noInput) (any, error) {
	problems := []string{}
	keys, err := h.activeKeys(ctx, p)
	if err != nil {
		return nil, err
	}
	var hasPublishable bool
	keyScopes := map[string]bool{}
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

	grants := map[string]bool{}
	for _, f := range []rbac.Feature{rbac.FeatureMatchmaker, rbac.FeatureP2PRelay, rbac.FeatureDedicatedServers} {
		on, err := h.d.RBAC.FeatureEnabled(ctx, p.TenantID, p.ProjectID, f)
		if err != nil {
			return nil, err
		}
		grants[string(f)] = on
	}
	if grants[string(rbac.FeatureMatchmaker)] && !keyScopes[tenant.ScopeMatchmaker] {
		problems = append(problems, "Matchmaking is granted, but no active API key has the matchmaker scope. /v1/matchmaker calls get 403.")
	}
	if grants[string(rbac.FeatureP2PRelay)] {
		switch {
		case !h.d.RelayEnabled || !h.d.RelayConfigured:
			problems = append(problems, "The relay is granted, but the server relay is off or not configured. /v1/relay is not available.")
		case !keyScopes[tenant.ScopeP2PRelay]:
			problems = append(problems, "The relay is granted, but no active API key has the p2p_relay scope.")
		}
	}
	if grants[string(rbac.FeatureDedicatedServers)] && !h.d.FleetEnabled {
		problems = append(problems, "Dedicated servers are granted, but FEATURE_FLEET_ENABLED is off on the server.")
	}

	var (
		qc           sqlcgen.GetTenantQuotaContextRow
		projects     int64
		openSessions int64
		configBytes  int
		steam        sqlcgen.GetProjectSteamAuthConfigForControlPanelRow
	)
	err = h.q(ctx, p, func(ctx context.Context, q *sqlcgen.Queries) error {
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
		return nil, err
	}
	limits, err := quota.ResolveSnapshot(int(qc.Tier), qc.Overrides)
	if err != nil {
		return nil, err
	}
	quotas := map[string]any{"enforced": qc.EnforceQuotas}
	if qc.EnforceQuotas {
		quotas[quota.AxisProjects] = quotaState(int64(limits.Projects), projects)
		quotas[quota.AxisPlayers] = quotaState(limits.Players, qc.PlayerCount)
		for _, axis := range []string{quota.AxisProjects, quota.AxisPlayers} {
			if quotas[axis] == "at_limit" {
				problems = append(problems, fmt.Sprintf("The Account Tenant is at its %s quota. New %s are refused.", axis, axis))
			}
		}
	}
	// open_sessions is a project quota, so its counts are returned.
	quotas[quota.AxisOpenSessions] = map[string]any{"current": openSessions, "limit": limits.OpenSessionsPerProject}

	steamConfigured := steam.SteamAppID != "" && steam.SteamKeyConfigured != nil && *steam.SteamKeyConfigured
	if steam.SteamAppID != "" && !steamConfigured {
		problems = append(problems, "Steam sign-in has an App ID but no Web API key, so it is off.")
	}
	origins := h.d.CORSAllowedOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	sort.Strings(problems)
	return map[string]any{
		"problems": problems,
		"checks": map[string]any{
			"api_keys":             keys[:min(len(keys), maxListRows)],
			"feature_grants":       grants,
			"server_switches":      map[string]bool{"fleet": h.d.FleetEnabled, "p2p_relay": h.d.RelayEnabled, "relay_configured": h.d.RelayConfigured},
			"cors_allowed_origins": origins,
			"steam_sign_in":        steamConfigured,
			"remote_config_bytes":  configBytes,
			"quotas":               quotas,
		},
	}, nil
}
