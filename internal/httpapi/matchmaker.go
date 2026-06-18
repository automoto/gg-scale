package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
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
		endUserID, ok := enduser.IDFromContext(ctx)
		if !ok {
			http.Error(w, "no end user", http.StatusUnauthorized)
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
		if d.RBAC != nil {
			allowed, aerr := d.RBAC.CanEndUser(ctx, tenantID, projectID, endUserID, rbac.ProjectDedicatedMatchmakingObject(projectID), rbac.ActionCreateTicket)
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
			EndUserID:  endUserID,
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
		ticket, err := d.Matchmaker.Get(r.Context(), id)
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
		err := d.Matchmaker.Cancel(r.Context(), id)
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
