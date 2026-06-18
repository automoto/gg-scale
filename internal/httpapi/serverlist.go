package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/serverlist"
	"github.com/ggscale/ggscale/internal/webutil"
)

type heartbeatRequest struct {
	AgonesName     string `json:"agones_name"`
	Fleet          string `json:"fleet"`
	Address        string `json:"address"`
	Region         string `json:"region"`
	Name           string `json:"name"`
	CurrentPlayers int    `json:"current_players"`
	MaxPlayers     int    `json:"max_players"`
	GameMode       string `json:"game_mode"`
	Level          string `json:"level"`
	Version        string `json:"version"`
}

type listServersResponse struct {
	Servers []serverlist.Server `json:"servers"`
}

// fleetHeartbeatHandler accepts a heartbeat from a game-server. The
// tenant is taken from the authenticated context, not the request body,
// so a tenant can't spoof another tenant's fleet.
func fleetHeartbeatHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.ServerList == nil {
			http.Error(w, "server list not configured", http.StatusServiceUnavailable)
			return
		}
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		req, err := webutil.DecodeJSON[heartbeatRequest](w, r, 8<<10)
		if err != nil {
			if errors.Is(err, webutil.ErrBodyTooLarge) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if req.AgonesName == "" || req.Fleet == "" || req.Address == "" {
			http.Error(w, "agones_name, fleet, and address are required", http.StatusBadRequest)
			return
		}
		if req.MaxPlayers <= 0 {
			http.Error(w, "max_players must be > 0", http.StatusBadRequest)
			return
		}
		if req.CurrentPlayers < 0 || req.CurrentPlayers > req.MaxPlayers {
			http.Error(w, "current_players must be in [0, max_players]", http.StatusBadRequest)
			return
		}
		err = d.ServerList.Submit(serverlist.Heartbeat{
			AgonesName:     req.AgonesName,
			Fleet:          req.Fleet,
			Address:        req.Address,
			Region:         req.Region,
			Name:           req.Name,
			CurrentPlayers: req.CurrentPlayers,
			MaxPlayers:     req.MaxPlayers,
			GameMode:       req.GameMode,
			Level:          req.Level,
			Version:        req.Version,
			TenantID:       tenantID,
		})
		if errors.Is(err, serverlist.ErrTenantLimitExceeded) {
			http.Error(w, "server list limit exceeded", http.StatusTooManyRequests)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// fleetServersListHandler returns the live servers for a fleet, scoped
// to the authenticated tenant.
func fleetServersListHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.ServerList == nil {
			http.Error(w, "server list not configured", http.StatusServiceUnavailable)
			return
		}
		tenantID, err := db.TenantFromContext(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		fleet := chi.URLParam(r, "fleet")
		if fleet == "" {
			http.Error(w, "fleet is required", http.StatusBadRequest)
			return
		}
		out := listServersResponse{Servers: d.ServerList.List(tenantID, fleet)}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
