package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	matchmakerCreateRate  = 2.0
	matchmakerCreateBurst = 5.0
	matchmakerCancelRate  = 5.0
	matchmakerCancelBurst = 10.0
)

type matchmakerTicketRequest struct {
	Fleet      string          `json:"fleet"`
	Region     string          `json:"region"`
	GameMode   string          `json:"game_mode"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

type matchmakerTicketResponse struct {
	ID           int64           `json:"id"`
	Status       string          `json:"status"`
	Region       string          `json:"region"`
	GameMode     string          `json:"game_mode"`
	Attributes   json.RawMessage `json:"attributes,omitempty"`
	MatchAddress string          `json:"match_address"`
	// ProtocolHint is the wire protocol the matched game-server listens
	// on (lower-cased: "tcp", "udp", "tcpudp"). Empty when the backend
	// can't determine it (older allocations, plugin backends that don't
	// surface it). Game-specific SDKs already know what to dial; this
	// field is for cross-game launchers, dashboards, and defense-in-depth.
	ProtocolHint string `json:"protocol_hint,omitempty"`
	CreatedAt    string `json:"created_at"`
	MatchedAt    string `json:"matched_at,omitempty"`
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
		if req.Fleet == "" {
			http.Error(w, "fleet is required", http.StatusBadRequest)
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
		enabled, ferr := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureDedicatedServers)
		if ferr != nil {
			http.Error(w, "feature check failed", http.StatusInternalServerError)
			return
		}
		if !enabled {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !allowMatchmakerAction(w, r, d, tenantID, projectID, playerID, "create", matchmakerCreateRate, matchmakerCreateBurst) {
			return
		}
		if banned, berr := playerTenantBanned(ctx, d, playerID); berr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		} else if banned {
			http.Error(w, "account banned", http.StatusForbidden)
			return
		}
		if d.Fleet == nil {
			http.Error(w, "fleet backend not configured", http.StatusServiceUnavailable)
			return
		}
		f, ferr := d.Fleet.Fleets().GetByName(ctx, projectID, req.Fleet)
		if errors.Is(ferr, fleet.ErrFleetNotFound) {
			http.Error(w, "unknown fleet", http.StatusBadRequest)
			return
		}
		if ferr != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		ticket, err := d.Matchmaker.Enqueue(ctx, matchmaker.EnqueueRequest{
			TenantID:   tenantID,
			ProjectID:  projectID,
			FleetID:    f.ID,
			PlayerID:   playerID,
			Region:     req.Region,
			GameMode:   req.GameMode,
			Attributes: req.Attributes,
		})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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
		writeTicket(w, ticket, http.StatusOK)
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
	resp := matchmakerTicketResponse{
		ID:           t.ID,
		Status:       string(t.Status),
		Region:       t.Region,
		GameMode:     t.GameMode,
		Attributes:   t.Attributes,
		MatchAddress: t.MatchAddress,
		ProtocolHint: t.MatchProtocol,
		CreatedAt:    t.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
	if t.MatchedAt != nil {
		resp.MatchedAt = t.MatchedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
