package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/enduser"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/matchmaker"
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
	CreatedAt    string          `json:"created_at"`
	MatchedAt    string          `json:"matched_at,omitempty"`
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

		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "body too large", http.StatusBadRequest)
			return
		}
		var req matchmakerTicketRequest
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
		}
		if len(req.Attributes) > 0 && !json.Valid(req.Attributes) {
			http.Error(w, "attributes must be valid JSON", http.StatusBadRequest)
			return
		}
		if req.Fleet == "" {
			http.Error(w, "fleet is required", http.StatusBadRequest)
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
		CreatedAt:    t.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
	if t.MatchedAt != nil {
		resp.MatchedAt = t.MatchedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
