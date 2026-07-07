package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/serverlist"
)

// heartbeatRequest fields are schema-optional so the handler owns the
// (cross-field) validation → 400, matching the pre-migration wire.
type heartbeatRequest struct {
	AgonesName     string `json:"agones_name,omitempty"`
	Fleet          string `json:"fleet,omitempty"`
	Address        string `json:"address,omitempty"`
	Region         string `json:"region,omitempty"`
	Name           string `json:"name,omitempty"`
	CurrentPlayers int    `json:"current_players,omitempty"`
	MaxPlayers     int    `json:"max_players,omitempty"`
	GameMode       string `json:"game_mode,omitempty"`
	Level          string `json:"level,omitempty"`
	Version        string `json:"version,omitempty"`
}

type listServersResponse struct {
	Servers []serverlist.Server `json:"servers"`
}

type heartbeatInput struct {
	Body heartbeatRequest
}

type fleetServersInput struct {
	Fleet string `path:"fleet"`
}

type fleetServersOutput struct {
	Body listServersResponse
}

// registerFleetHeartbeat registers the server-tier heartbeat. Body is capped
// at 8 KiB (oversize → 413).
func registerFleetHeartbeat(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "fleetHeartbeat",
		Method:        http.MethodPost,
		Path:          "/v1/fleets/heartbeat",
		Summary:       "Game-server liveness heartbeat",
		Tags:          []string{"/v1"},
		Security:      apiKeySecurity,
		DefaultStatus: http.StatusNoContent,
		MaxBodyBytes:  8 << 10,
	}, fleetHeartbeat(d))
}

func registerFleetServersList(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "fleetServersList",
		Method:      http.MethodGet,
		Path:        "/v1/fleets/{fleet}/servers",
		Summary:     "List live servers in a fleet",
		Tags:        []string{"/v1"},
		Security:    playerSecurity,
	}, fleetServersList(d))
}

// fleetHeartbeat accepts a heartbeat from a game-server. The tenant is taken
// from the authenticated context, not the request body, so a tenant can't
// spoof another tenant's fleet.
func fleetHeartbeat(d Deps) func(context.Context, *heartbeatInput) (*struct{}, error) {
	return func(ctx context.Context, in *heartbeatInput) (*struct{}, error) {
		if d.ServerList == nil {
			return nil, huma.Error503ServiceUnavailable("server list not configured")
		}
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		req := in.Body
		if req.AgonesName == "" || req.Fleet == "" || req.Address == "" {
			return nil, huma.Error400BadRequest("agones_name, fleet, and address are required")
		}
		if req.MaxPlayers <= 0 {
			return nil, huma.Error400BadRequest("max_players must be > 0")
		}
		if req.CurrentPlayers < 0 || req.CurrentPlayers > req.MaxPlayers {
			return nil, huma.Error400BadRequest("current_players must be in [0, max_players]")
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
			return nil, huma.Error429TooManyRequests("server list limit exceeded")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		return nil, nil
	}
}

// fleetServersList returns the live servers for a fleet, scoped to the
// authenticated tenant.
func fleetServersList(d Deps) func(context.Context, *fleetServersInput) (*fleetServersOutput, error) {
	return func(ctx context.Context, in *fleetServersInput) (*fleetServersOutput, error) {
		if d.ServerList == nil {
			return nil, huma.Error503ServiceUnavailable("server list not configured")
		}
		tenantID, err := db.TenantFromContext(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if in.Fleet == "" {
			return nil, huma.Error400BadRequest("fleet is required")
		}
		return &fleetServersOutput{Body: listServersResponse{Servers: d.ServerList.List(tenantID, in.Fleet)}}, nil
	}
}
