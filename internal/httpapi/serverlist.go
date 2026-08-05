package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/serverlist"
)

// heartbeatRequest fields are schema-optional so the handler owns the
// (cross-field) validation → 400, matching the pre-migration wire.
type heartbeatRequest struct {
	AgonesName     string `json:"agones_name,omitempty" example:"gameserver-abc12"`
	Fleet          string `json:"fleet,omitempty" example:"default"`
	Address        string `json:"address,omitempty" example:"203.0.113.10:7777"`
	Region         string `json:"region,omitempty" example:"us-east-1"`
	Name           string `json:"name,omitempty" example:"us-east-1-a1"`
	CurrentPlayers int    `json:"current_players,omitempty" example:"12"`
	MaxPlayers     int    `json:"max_players,omitempty" example:"16"`
	GameMode       string `json:"game_mode,omitempty" example:"ctf"`
	Level          string `json:"level,omitempty" example:"arena-2"`
	Version        string `json:"version,omitempty" example:"1.4.2"`
}

type listServersResponse struct {
	Servers []serverlist.Server `json:"servers"`
}

type heartbeatInput struct {
	Body heartbeatRequest
}

type fleetServersInput struct {
	Fleet string `path:"fleet" example:"default"`
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
		Tags:          []string{"Game Server Fleet"},
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
		Tags:        []string{"Game Server Fleet"},
		Security:    playerSecurity,
	}, fleetServersList(d))
}

// requireDedicatedServers gates fleet endpoints behind the dedicated_servers
// entitlement, matching the matchmaker fleet path — an API-key fleet scope
// alone must not keep a fleet running after the entitlement is revoked.
func requireDedicatedServers(ctx context.Context, d Deps, tenantID, projectID int64) error {
	if d.RBAC == nil {
		return huma.Error500InternalServerError("authorization unavailable")
	}
	enabled, err := d.RBAC.FeatureEnabled(ctx, tenantID, projectID, rbac.FeatureDedicatedServers)
	if err != nil {
		return huma.Error500InternalServerError("feature check failed")
	}
	if !enabled {
		return huma.Error403Forbidden("forbidden")
	}
	return nil
}

// fleetHeartbeat accepts a heartbeat from a game-server. The tenant is taken
// from the authenticated context, not the request body, so a tenant can't
// spoof another tenant's fleet.
func fleetHeartbeat(d Deps) func(context.Context, *heartbeatInput) (*struct{}, error) {
	return func(ctx context.Context, in *heartbeatInput) (*struct{}, error) {
		if d.ServerList == nil {
			return nil, huma.Error503ServiceUnavailable("server list not configured")
		}
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		if ferr := requireDedicatedServers(ctx, d, tenantID, projectID); ferr != nil {
			return nil, ferr
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
		projectID, tenantID, err := pinnedProject(ctx)
		if err != nil {
			return nil, err
		}
		if ferr := requireDedicatedServers(ctx, d, tenantID, projectID); ferr != nil {
			return nil, ferr
		}
		if in.Fleet == "" {
			return nil, huma.Error400BadRequest("fleet is required")
		}
		return &fleetServersOutput{Body: listServersResponse{Servers: d.ServerList.List(tenantID, in.Fleet)}}, nil
	}
}
