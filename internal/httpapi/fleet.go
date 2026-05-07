package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
)

type fleetRegisterRequest struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	Version    string `json:"version"`
	Region     string `json:"region"`
	MaxPlayers int    `json:"max_players"`
}

type fleetServerView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Address       string `json:"address"`
	Version       string `json:"version"`
	Region        string `json:"region"`
	MaxPlayers    int    `json:"max_players"`
	LastHeartbeat string `json:"last_heartbeat"`
}

// POST /v1/fleet/servers — register a game server.
func fleetRegisterHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet == nil {
			http.Error(w, "fleet disabled", http.StatusServiceUnavailable)
			return
		}
		var req fleetRegisterRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Address) == "" {
			http.Error(w, "address required", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		id, err := d.Fleet.Register(fleet.RegisterParams{
			TenantID:   tenantID,
			ProjectID:  projectID,
			Name:       req.Name,
			Address:    req.Address,
			Version:    req.Version,
			Region:     req.Region,
			MaxPlayers: req.MaxPlayers,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]string{"id": id.String()})
	}
}

// PUT /v1/fleet/servers/{id}/heartbeat — refresh a server's TTL.
func fleetHeartbeatHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet == nil {
			http.Error(w, "fleet disabled", http.StatusServiceUnavailable)
			return
		}
		id, ok := pathUUID(r, "id")
		if !ok {
			http.Error(w, "invalid server id", http.StatusBadRequest)
			return
		}
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		if err := d.Fleet.Heartbeat(tenantID, id); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// DELETE /v1/fleet/servers/{id} — deregister.
func fleetDeregisterHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet == nil {
			http.Error(w, "fleet disabled", http.StatusServiceUnavailable)
			return
		}
		id, ok := pathUUID(r, "id")
		if !ok {
			http.Error(w, "invalid server id", http.StatusBadRequest)
			return
		}
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		if err := d.Fleet.Deregister(tenantID, id); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// GET /v1/fleet/servers — list active servers in the caller's project.
func fleetListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet == nil {
			writeJSON(w, map[string]any{"servers": []fleetServerView{}})
			return
		}
		ctx := r.Context()
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			http.Error(w, "no tenant", http.StatusUnauthorized)
			return
		}
		projectID, ok := db.ProjectFromContext(ctx)
		if !ok {
			http.Error(w, "api key has no project pin", http.StatusBadRequest)
			return
		}

		active := d.Fleet.List(tenantID, projectID)
		out := make([]fleetServerView, 0, len(active))
		for _, s := range active {
			out = append(out, fleetServerView{
				ID:            s.ID.String(),
				Name:          s.Name,
				Address:       s.Address,
				Version:       s.Version,
				Region:        s.Region,
				MaxPlayers:    s.MaxPlayers,
				LastHeartbeat: s.LastHeartbeat.UTC().Format("2006-01-02T15:04:05Z07:00"),
			})
		}
		writeJSON(w, map[string]any{"servers": out})
	}
}

func pathUUID(r *http.Request, name string) (uuid.UUID, bool) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
