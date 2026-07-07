package httpapi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/gamesession"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/matchmaker/query"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/webutil"
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
	// field is for cross-game launchers, dashboards, and defense-in-depth.
	ProtocolHint string `json:"protocol_hint,omitempty"`
	// SessionID / JoinCode are set for matched game_session tickets.
	SessionID string `json:"session_id,omitempty"`
	JoinCode  string `json:"join_code,omitempty"`
	// Users is the match roster, populated once the ticket is matched (and
	// while the match record is within its retention window).
	Users     []matchmaker.RosterEntry `json:"users,omitempty"`
	CreatedAt string                   `json:"created_at"`
	MatchedAt string                   `json:"matched_at,omitempty"`
	ExpiresAt string                   `json:"expires_at,omitempty"`
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

func matchmakerCreateTicketHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "no project", http.StatusBadRequest)
			return
		}
		playerID, ok := playerauth.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}

		req, err := webutil.DecodeJSON[matchmakerTicketRequest](w, r, 64<<10)
		if err != nil {
			if errors.Is(err, webutil.ErrBodyTooLarge) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if len(req.Attributes) > 0 && !json.Valid(req.Attributes) {
			http.Error(w, "attributes must be valid JSON", http.StatusBadRequest)
			return
		}
		mode, problem := resolveTicketMode(&req)
		if problem != "" {
			http.Error(w, problem, http.StatusBadRequest)
			return
		}
		if d.RBAC == nil {
			http.Error(w, "authorization unavailable", http.StatusInternalServerError)
			return
		}
		allowed, aerr := d.RBAC.CanPlayer(tenantID, playerID, rbac.ProjectDedicatedMatchmakingObject(projectID), rbac.ActionCreateTicket)
		if aerr != nil {
			http.Error(w, "authorization check failed", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		enabled, ferr := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureMatchmaker)
		if ferr != nil {
			http.Error(w, "feature check failed", http.StatusInternalServerError)
			return
		}
		if !enabled {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var fleetID int64
		if mode == matchmaker.ModeFleetAllocation {
			// Fleet-backed tickets allocate dedicated servers, which stays
			// behind the fleet key scope and the dedicated_servers
			// entitlement.
			key, ok := tenant.APIKeyFromContext(ctx)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !key.HasScope(tenant.ScopeFleet) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			dedicated, derr := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureDedicatedServers)
			if derr != nil {
				http.Error(w, "feature check failed", http.StatusInternalServerError)
				return
			}
			if !dedicated {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if d.Fleet == nil {
				http.Error(w, "fleet backend not configured", http.StatusServiceUnavailable)
				return
			}
			f, gerr := d.Fleet.Fleets().GetByName(ctx, projectID, req.Fleet)
			if errors.Is(gerr, fleet.ErrFleetNotFound) {
				http.Error(w, "unknown fleet", http.StatusBadRequest)
				return
			}
			if gerr != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			fleetID = f.ID
		}
		if !allowMatchmakerAction(w, r, d, tenantID, projectID, playerID, "create", matchmakerCreateRate, matchmakerCreateBurst) {
			return
		}
		// d.Pool is required to mount the router; nil only in handler unit
		// tests, where the ban check has nothing to consult anyway.
		if d.Pool != nil {
			if banned, berr := playerTenantBanned(ctx, d, playerID); berr != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			} else if banned {
				http.Error(w, "account banned", http.StatusForbidden)
				return
			}
		}

		enq := matchmaker.EnqueueRequest{
			TenantID:          tenantID,
			ProjectID:         projectID,
			FleetID:           fleetID,
			PlayerID:          playerID,
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
			MaxActive:         d.MatchmakerMaxTicketsPerPlayer,
		}
		if d.MatchmakerTicketTTL > 0 {
			exp := time.Now().UTC().Add(d.MatchmakerTicketTTL)
			enq.ExpiresAt = &exp
		}
		ticket, err := d.Matchmaker.Enqueue(ctx, enq)
		if errors.Is(err, matchmaker.ErrTicketLimit) {
			http.Error(w, "too many queued tickets", http.StatusTooManyRequests)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		d.Metrics.MatchmakerTicket()
		writeTicket(w, ticket, http.StatusCreated)
	}
}

func matchmakerGetTicketHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTicketID(w, r)
		if !ok {
			return
		}
		playerID, ok := playerauth.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		ticket, err := d.Matchmaker.Get(r.Context(), id, playerID)
		if errors.Is(err, matchmaker.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Matched tickets carry their roster (and session/allocation
		// details) so a missed WebSocket event is recoverable by polling.
		// A match past its retention window degrades to the bare ticket.
		var match *matchmaker.Match
		if ticket.MatchID != "" {
			m, merr := d.Matchmaker.GetMatch(r.Context(), ticket.MatchID)
			if merr != nil && !errors.Is(merr, matchmaker.ErrNotFound) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			match = m
		}
		writeMatchedTicket(w, ticket, match, http.StatusOK)
	}
}

func matchmakerCancelTicketHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTicketID(w, r)
		if !ok {
			return
		}
		playerID, ok := playerauth.IDFromContext(r.Context())
		if !ok {
			http.Error(w, "no player", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(r.Context())
		if !ok {
			http.Error(w, "no project", http.StatusBadRequest)
			return
		}
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !allowMatchmakerAction(w, r, d, tenantID, projectID, playerID, "cancel", matchmakerCancelRate, matchmakerCancelBurst) {
			return
		}
		err = d.Matchmaker.Cancel(r.Context(), id, playerID)
		switch {
		case errors.Is(err, matchmaker.ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, matchmaker.ErrAlreadyTerminal):
			http.Error(w, "ticket already finalised", http.StatusConflict)
		case err != nil:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func allowMatchmakerAction(w http.ResponseWriter, r *http.Request, d Deps, tenantID, projectID, playerID int64, action string, rate, burst float64) bool {
	if d.Limiter == nil {
		http.Error(w, "rate limiter unavailable", http.StatusInternalServerError)
		return false
	}
	key := fmt.Sprintf("ratelimit:matchmaker:%s:%d:%d:%d", action, tenantID, projectID, playerID)
	decision, err := d.Limiter.Allow(r.Context(), key, rate, burst)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if decision.Allowed {
		return true
	}
	retrySec := int(math.Ceil(decision.RetryAfter.Seconds()))
	if retrySec < 1 {
		retrySec = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retrySec))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":               "rate_limit_exceeded",
		"retry_after_seconds": retrySec,
	})
	return false
}

func parseTicketID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func writeTicket(w http.ResponseWriter, t *matchmaker.Ticket, status int) {
	writeMatchedTicket(w, t, nil, status)
}

func writeMatchedTicket(w http.ResponseWriter, t *matchmaker.Ticket, m *matchmaker.Match, status int) {
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
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
