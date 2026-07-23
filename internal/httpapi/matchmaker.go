package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/gamesession"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/matchmaker/query"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

const (
	matchmakerCreateRate  = 2.0
	matchmakerCreateBurst = 5.0
	matchmakerCancelRate  = 5.0
	matchmakerCancelBurst = 10.0
)

type matchmakerTicketRequest struct {
	// Mode selects the match result: match_only (roster + match id),
	// game_session (created session), or fleet_allocation (dedicated
	// server). Omitted mode is inferred: fleet present → fleet_allocation,
	// absent → match_only.
	Mode              string             `json:"mode,omitempty"`
	Fleet             string             `json:"fleet,omitempty"`
	Region            string             `json:"region,omitempty"`
	AllowCrossRegion  *bool              `json:"allow_cross_region,omitempty"`
	GameMode          string             `json:"game_mode,omitempty"`
	MinCount          int                `json:"min_count,omitempty"`
	MaxCount          int                `json:"max_count,omitempty"`
	CountMultiple     int                `json:"count_multiple,omitempty"`
	Query             string             `json:"query,omitempty"`
	StringProperties  map[string]string  `json:"string_properties,omitempty"`
	NumericProperties map[string]float64 `json:"numeric_properties,omitempty"`
	Attributes        json.RawMessage    `json:"attributes,omitempty"`
}

type matchmakerTicketResponse struct {
	ID                int64              `json:"id"`
	Status            string             `json:"status"`
	Mode              string             `json:"mode"`
	Region            string             `json:"region"`
	AllowCrossRegion  bool               `json:"allow_cross_region"`
	GameMode          string             `json:"game_mode"`
	MinCount          int                `json:"min_count"`
	MaxCount          int                `json:"max_count"`
	CountMultiple     int                `json:"count_multiple"`
	Query             string             `json:"query,omitempty"`
	StringProperties  map[string]string  `json:"string_properties,omitempty"`
	NumericProperties map[string]float64 `json:"numeric_properties,omitempty"`
	Attributes        json.RawMessage    `json:"attributes,omitempty"`
	MatchID           string             `json:"match_id,omitempty"`
	MatchAddress      string             `json:"match_address"`
	// ProtocolHint is the wire protocol the matched game-server listens
	// on (lower-cased: "tcp", "udp", "tcpudp"). Empty when the backend
	// can't determine it (older allocations, plugin backends that don't
	// surface it). Game-specific SDKs already know what to dial; this
	// field is for cross-game launchers, control panels, and defense-in-depth.
	ProtocolHint string `json:"protocol_hint,omitempty"`
	// SessionID / JoinCode are set for matched game_session tickets.
	SessionID string `json:"session_id,omitempty"`
	JoinCode  string `json:"join_code,omitempty"`
	// HostPlayerID is the player peers connect to for matched match_only and
	// game_session tickets (the group's oldest ticket). Absent for
	// fleet_allocation, which resolves to a dedicated server address.
	HostPlayerID int64 `json:"host_player_id,omitempty"`
	// Users is the match roster, populated once the ticket is matched (and
	// while the match record is within its retention window).
	Users []matchmaker.RosterEntry `json:"users,omitempty"`
	// FailureReason is a machine-readable reason a failed ticket ended that
	// way. Present only for failed tickets. Known values: "expired",
	// "attempts_exhausted"; treat as an open enum — more may be added.
	FailureReason string `json:"failure_reason,omitempty"`
	CreatedAt     string `json:"created_at"`
	MatchedAt     string `json:"matched_at,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
}

// resolveTicketMode applies the omitted-mode inference rule and validates
// the request's mode-dependent and count fields, normalising defaults in
// place. Returns a non-empty problem string on invalid input.
func resolveTicketMode(req *matchmakerTicketRequest) (matchmaker.Mode, string) {
	mode := inferTicketMode(req)
	fleetMode := mode == matchmaker.ModeFleetAllocation
	switch {
	case !matchmaker.ValidMode(mode):
		return "", "unknown mode"
	case fleetMode && req.Fleet == "":
		return "", "fleet is required for fleet_allocation"
	case fleetMode && req.AllowCrossRegion != nil:
		return "", "allow_cross_region is not supported for fleet_allocation"
	case !fleetMode && req.Fleet != "":
		return "", "fleet is only valid for fleet_allocation"
	}
	if problem := normalizeTicketCounts(req, mode); problem != "" {
		return "", problem
	}
	return mode, validateTicketCriteria(req)
}

const (
	maxPropsPerKind    = 16
	maxPropValueLength = 128
	// maxAttributesBytes caps the opaque per-ticket attributes blob. It is
	// echoed to every matched peer via the roster, so bound it well below the
	// 64 KiB request-body ceiling (which is not a per-field limit).
	maxAttributesBytes = 4 << 10
)

// validateTicketCriteria checks the query expression and property maps.
// region and game_mode are exposed to queries as implicit read-only
// properties, so user-supplied properties may not shadow them.
func validateTicketCriteria(req *matchmakerTicketRequest) string {
	if _, err := query.Parse(req.Query); err != nil {
		return err.Error()
	}
	if len(req.StringProperties) > maxPropsPerKind || len(req.NumericProperties) > maxPropsPerKind {
		return fmt.Sprintf("at most %d properties per kind", maxPropsPerKind)
	}
	for k, v := range req.StringProperties {
		if problem := validatePropKey(k); problem != "" {
			return problem
		}
		if len(v) > maxPropValueLength {
			return fmt.Sprintf("string property %q value exceeds %d bytes", k, maxPropValueLength)
		}
	}
	for k := range req.NumericProperties {
		if problem := validatePropKey(k); problem != "" {
			return problem
		}
	}
	return ""
}

func validatePropKey(k string) string {
	if !query.ValidKey(k) {
		return fmt.Sprintf("invalid property key %q", k)
	}
	if k == "region" || k == "game_mode" {
		return fmt.Sprintf("property key %q is reserved (set the ticket field instead)", k)
	}
	return ""
}

// inferTicketMode returns the explicit mode, or infers one: fleet present →
// fleet_allocation, absent → match_only.
func inferTicketMode(req *matchmakerTicketRequest) matchmaker.Mode {
	switch {
	case req.Mode != "":
		return matchmaker.Mode(req.Mode)
	case req.Fleet != "":
		return matchmaker.ModeFleetAllocation
	default:
		return matchmaker.ModeMatchOnly
	}
}

// normalizeTicketCounts fills count defaults (min 1, max = min, multiple 1)
// and rejects ranges no roster size can ever satisfy. mode bounds the upper
// limit: game_session rosters can't exceed what the session store admits, and
// every mode is capped so the int32 ticket columns can't overflow on persist.
func normalizeTicketCounts(req *matchmakerTicketRequest, mode matchmaker.Mode) string {
	if req.MinCount < 0 || req.MaxCount < 0 || req.CountMultiple < 0 {
		return "counts must be positive"
	}
	req.MinCount = cmp.Or(req.MinCount, 1)
	req.MaxCount = cmp.Or(req.MaxCount, req.MinCount)
	req.CountMultiple = cmp.Or(req.CountMultiple, 1)
	limit := math.MaxInt32
	if mode == matchmaker.ModeGameSession {
		// A game_session ticket accepted above the session cap would form a
		// roster the session store rejects at commit and then loop through
		// the attempt counter forever, so bound it at enqueue instead.
		limit = gamesession.MaxPlayersLimit
	}
	switch {
	case req.MinCount > req.MaxCount:
		return "min_count must not exceed max_count"
	case req.MaxCount > limit || req.CountMultiple > limit:
		return fmt.Sprintf("counts must not exceed %d", limit)
	case (req.MaxCount/req.CountMultiple)*req.CountMultiple < req.MinCount:
		// No size in [min_count, max_count] is a multiple of
		// count_multiple, so the ticket could never commit.
		return "no roster size in [min_count, max_count] is a multiple of count_multiple"
	}
	return ""
}

type matchmakerCreateInput struct {
	Body matchmakerTicketRequest
}

type matchmakerTicketOutput struct {
	Body matchmakerTicketResponse
}

type matchmakerTicketIDInput struct {
	ID int64 `path:"id"`
}

func registerMatchmakerRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "createMatchmakerTicket",
		Method:        http.MethodPost,
		Path:          "/v1/matchmaker/tickets",
		Summary:       "Create a matchmaking ticket",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusCreated,
		MaxBodyBytes:  64 << 10,
	}, matchmakerCreateTicket(d))

	huma.Register(api, huma.Operation{
		OperationID: "getMatchmakerTicket",
		Method:      http.MethodGet,
		Path:        "/v1/matchmaker/tickets/{id}",
		Summary:     "Get a matchmaking ticket",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, matchmakerGetTicket(d))

	huma.Register(api, huma.Operation{
		OperationID:   "cancelMatchmakerTicket",
		Method:        http.MethodDelete,
		Path:          "/v1/matchmaker/tickets/{id}",
		Summary:       "Cancel a matchmaking ticket",
		Tags:          []string{"/v1"},
		Security:      playerSecurity,
		DefaultStatus: http.StatusNoContent,
	}, matchmakerCancelTicket(d))
}

// matchmakerContext holds the identifiers every ticket operation resolves from
// the request context.
type matchmakerContext struct {
	tenantID  int64
	projectID int64
	playerID  int64
}

// resolveMatchmakerContext extracts the tenant, project, and player from the
// request context, returning the same status codes the ticket routes expose.
func resolveMatchmakerContext(ctx context.Context) (matchmakerContext, error) {
	tenantID, err := db.TenantFromContext(ctx)
	if err != nil {
		return matchmakerContext{}, huma.Error500InternalServerError("internal error")
	}
	projectID, ok := db.ProjectFromContext(ctx)
	if !ok {
		return matchmakerContext{}, huma.Error400BadRequest("no project")
	}
	playerID, ok := playerauth.IDFromContext(ctx)
	if !ok {
		return matchmakerContext{}, huma.Error401Unauthorized("no player")
	}
	return matchmakerContext{tenantID: tenantID, projectID: projectID, playerID: playerID}, nil
}

// authorizeMatchmaker runs the player RBAC gate and the matchmaker feature
// switch shared by every ticket the player creates.
func authorizeMatchmaker(ctx context.Context, d Deps, mc matchmakerContext) error {
	if d.RBAC == nil {
		return huma.Error500InternalServerError("authorization unavailable")
	}
	allowed, err := d.RBAC.CanPlayer(mc.tenantID, mc.playerID, rbac.ProjectDedicatedMatchmakingObject(mc.projectID), rbac.ActionCreateTicket)
	if err != nil {
		return huma.Error500InternalServerError("authorization check failed")
	}
	if !allowed {
		return huma.Error403Forbidden("forbidden")
	}
	enabled, err := d.RBAC.FeatureEnabled(ctx, mc.tenantID, mc.projectID, rbac.FeatureMatchmaker)
	if err != nil {
		return huma.Error500InternalServerError("feature check failed")
	}
	if !enabled {
		return huma.Error403Forbidden("forbidden")
	}
	return nil
}

// resolveFleetForTicket gates and resolves the fleet a fleet_allocation ticket
// targets. Fleet-backed tickets allocate dedicated servers, which stays behind
// the fleet key scope and the dedicated_servers entitlement.
func resolveFleetForTicket(ctx context.Context, d Deps, mc matchmakerContext, fleetName string) (int64, error) {
	key, ok := tenant.APIKeyFromContext(ctx)
	if !ok {
		return 0, huma.Error401Unauthorized("unauthorized")
	}
	if !key.HasScope(tenant.ScopeFleet) {
		return 0, huma.Error403Forbidden("forbidden")
	}
	dedicated, err := d.RBAC.FeatureEnabled(ctx, mc.tenantID, mc.projectID, rbac.FeatureDedicatedServers)
	if err != nil {
		return 0, huma.Error500InternalServerError("feature check failed")
	}
	if !dedicated {
		return 0, huma.Error403Forbidden("forbidden")
	}
	if d.Fleet == nil {
		return 0, huma.Error503ServiceUnavailable("fleet backend not configured")
	}
	f, err := d.Fleet.Fleets().GetByName(ctx, mc.projectID, fleetName)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		return 0, huma.Error400BadRequest("unknown fleet")
	}
	if err != nil {
		return 0, huma.Error500InternalServerError("internal error")
	}
	return f.ID, nil
}

func matchmakerCreateTicket(d Deps) func(context.Context, *matchmakerCreateInput) (*matchmakerTicketOutput, error) {
	return func(ctx context.Context, in *matchmakerCreateInput) (*matchmakerTicketOutput, error) {
		mc, err := resolveMatchmakerContext(ctx)
		if err != nil {
			return nil, err
		}

		req := in.Body
		if len(req.Attributes) > maxAttributesBytes {
			return nil, huma.Error400BadRequest(fmt.Sprintf("attributes must not exceed %d bytes", maxAttributesBytes))
		}
		if len(req.Attributes) > 0 && !json.Valid(req.Attributes) {
			return nil, huma.Error400BadRequest("attributes must be valid JSON")
		}
		mode, problem := resolveTicketMode(&req)
		if problem != "" {
			return nil, huma.Error400BadRequest(problem)
		}
		if aerr := authorizeMatchmaker(ctx, d, mc); aerr != nil {
			return nil, aerr
		}

		var fleetID int64
		if mode == matchmaker.ModeFleetAllocation {
			fleetID, err = resolveFleetForTicket(ctx, d, mc, req.Fleet)
			if err != nil {
				return nil, err
			}
		}

		if aerr := allowMatchmakerAction(ctx, d, mc.tenantID, mc.projectID, mc.playerID, "create", matchmakerCreateRate, matchmakerCreateBurst); aerr != nil {
			return nil, aerr
		}
		// d.Pool is required to mount the router; nil only in handler unit
		// tests, where the ban check has nothing to consult anyway.
		if d.Pool != nil {
			if banned, berr := playerTenantBanned(ctx, d, mc.playerID); berr != nil {
				return nil, huma.Error500InternalServerError("internal error")
			} else if banned {
				return nil, huma.Error403Forbidden("account banned")
			}
		}

		enq := matchmaker.EnqueueRequest{
			TenantID:          mc.tenantID,
			ProjectID:         mc.projectID,
			FleetID:           fleetID,
			PlayerID:          mc.playerID,
			Mode:              mode,
			Region:            req.Region,
			GameMode:          req.GameMode,
			Attributes:        req.Attributes,
			MinCount:          req.MinCount,
			MaxCount:          req.MaxCount,
			CountMultiple:     req.CountMultiple,
			AllowCrossRegion:  req.AllowCrossRegion == nil || *req.AllowCrossRegion,
			Query:             req.Query,
			StringProperties:  req.StringProperties,
			NumericProperties: req.NumericProperties,
		}
		if d.MatchmakerTicketTTL > 0 {
			exp := time.Now().UTC().Add(d.MatchmakerTicketTTL)
			enq.ExpiresAt = &exp
		}
		ticket, err := d.Matchmaker.Enqueue(ctx, enq)
		var active *matchmaker.TicketActiveError
		if errors.As(err, &active) {
			return nil, huma.Error409Conflict("ticket_already_active", &huma.ErrorDetail{
				Message:  "player already has an active matchmaking ticket in this project",
				Location: "active_ticket_id",
				Value:    active.ActiveTicketID,
			})
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		d.Metrics.MatchmakerTicket()
		return &matchmakerTicketOutput{Body: ticketResponse(ticket, nil)}, nil
	}
}

func matchmakerGetTicket(d Deps) func(context.Context, *matchmakerTicketIDInput) (*matchmakerTicketOutput, error) {
	return func(ctx context.Context, in *matchmakerTicketIDInput) (*matchmakerTicketOutput, error) {
		playerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		ticket, err := d.Matchmaker.Get(ctx, in.ID, playerID)
		if errors.Is(err, matchmaker.ErrNotFound) {
			return nil, huma.Error404NotFound("not found")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		// Matched tickets carry their roster (and session/allocation
		// details) so a missed WebSocket event is recoverable by polling.
		// A match past its retention window degrades to the bare ticket.
		var match *matchmaker.Match
		if ticket.MatchID != "" {
			m, merr := d.Matchmaker.ClaimMatch(ctx, ticket.MatchID)
			if merr != nil && !errors.Is(merr, matchmaker.ErrNotFound) {
				return nil, huma.Error500InternalServerError("internal error")
			}
			match = m
		}
		return &matchmakerTicketOutput{Body: ticketResponse(ticket, match)}, nil
	}
}

func matchmakerCancelTicket(d Deps) func(context.Context, *matchmakerTicketIDInput) (*struct{}, error) {
	return func(ctx context.Context, in *matchmakerTicketIDInput) (*struct{}, error) {
		playerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("no player")
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			return nil, huma.Error400BadRequest("no project")
		}
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if aerr := allowMatchmakerAction(ctx, d, tenantID, projectID, playerID, "cancel", matchmakerCancelRate, matchmakerCancelBurst); aerr != nil {
			return nil, aerr
		}
		switch err = d.Matchmaker.Cancel(ctx, in.ID, playerID); {
		case errors.Is(err, matchmaker.ErrNotFound):
			return nil, huma.Error404NotFound("not found")
		case errors.Is(err, matchmaker.ErrAlreadyTerminal):
			return nil, huma.Error409Conflict("ticket already finalised")
		case err != nil:
			return nil, huma.Error500InternalServerError("internal error")
		}
		return nil, nil
	}
}

// allowMatchmakerAction enforces the per-action token bucket. On denial it
// returns a 429 problem+json carrying the canonical Retry-After header.
func allowMatchmakerAction(ctx context.Context, d Deps, tenantID, projectID, playerID int64, action string, rate, burst float64) error {
	if d.Limiter == nil {
		return huma.Error500InternalServerError("rate limiter unavailable")
	}
	key := fmt.Sprintf("ratelimit:matchmaker:%s:%d:%d:%d", action, tenantID, projectID, playerID)
	decision, err := d.Limiter.Allow(ctx, key, rate, burst)
	if err != nil {
		return huma.Error500InternalServerError("internal error")
	}
	if decision.Allowed {
		return nil
	}
	retrySec := int(math.Ceil(decision.RetryAfter.Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	return huma.ErrorWithHeaders(
		huma.Error429TooManyRequests("rate_limit_exceeded"),
		http.Header{"Retry-After": []string{strconv.Itoa(retrySec)}},
	)
}

func ticketResponse(t *matchmaker.Ticket, m *matchmaker.Match) matchmakerTicketResponse {
	resp := matchmakerTicketResponse{
		ID:                t.ID,
		Status:            string(t.Status),
		Mode:              string(t.Mode),
		Region:            t.Region,
		AllowCrossRegion:  t.AllowCrossRegion,
		GameMode:          t.GameMode,
		MinCount:          t.MinCount,
		MaxCount:          t.MaxCount,
		CountMultiple:     t.CountMultiple,
		Query:             t.Query,
		StringProperties:  t.StringProperties,
		NumericProperties: t.NumericProperties,
		Attributes:        t.Attributes,
		MatchID:           t.MatchID,
		MatchAddress:      t.MatchAddress,
		ProtocolHint:      t.MatchProtocol,
		CreatedAt:         t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.Status == matchmaker.StatusFailed {
		resp.FailureReason = t.FailureReason
	}
	if t.MatchedAt != nil {
		resp.MatchedAt = t.MatchedAt.UTC().Format(time.RFC3339Nano)
	}
	if t.ExpiresAt != nil {
		resp.ExpiresAt = t.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if m != nil {
		resp.Users = m.Roster
		resp.SessionID = m.SessionID
		resp.JoinCode = m.JoinCode
		resp.HostPlayerID = m.HostPlayerID
	}
	return resp
}
