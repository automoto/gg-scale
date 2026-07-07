package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/tenant"
)

func requireAPIKeyPermission(d Deps, obj, act string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d.RBAC == nil {
				http.Error(w, "authorization unavailable", http.StatusInternalServerError)
				return
			}
			key, ok := tenant.APIKeyFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := d.RBAC.CanAPIKey(key, obj, act)
			if err != nil {
				http.Error(w, "authorization check failed", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func mountFriendRoutes(r chi.Router, d Deps) {
	r.Route("/friends", func(r chi.Router) {
		r.Get("/", friendsListHandler(d))
		r.Post("/{player_id}/request", friendRequestHandler(d))
		r.Post("/{player_id}/accept", friendAcceptHandler(d))
		r.Post("/{player_id}/reject", friendRejectHandler(d))
		r.Post("/{player_id}/block", friendBlockHandler(d))
		r.Post("/{player_id}/unblock", friendUnblockHandler(d))
		r.Delete("/{player_id}", friendDeleteHandler(d))
	})
}

func mountRealtimeRoutes(r chi.Router, d Deps) {
	if d.Hub == nil {
		return
	}
	r.Get("/ws", realtime.ServeWS(realtime.Options{
		Hub:          d.Hub,
		Cache:        d.Cache,
		MaxPerTenant: d.RealtimeMaxPerTenant,
		MaxPerPlayer: d.RealtimeMaxPerPlayer,
	}))
}

// mountFleetHeartbeatRoute is server-tier (secret api_key). Game-servers
// authenticate with their secret key and POST liveness + player count.
func mountFleetHeartbeatRoute(r chi.Router, d Deps) {
	if d.ServerList == nil {
		return
	}
	r.With(tenant.RequireKeyScope(tenant.ScopeFleet)).Post("/fleets/heartbeat", fleetHeartbeatHandler(d))
}

// mountFleetListRoute is player tier. Any authenticated session can
// browse the live server list for its tenant.
func mountFleetListRoute(r chi.Router, d Deps) {
	if d.ServerList == nil {
		return
	}
	r.With(tenant.RequireKeyScope(tenant.ScopeFleet)).Get("/fleets/{fleet}/servers", fleetServersListHandler(d))
}

func mountMatchmakerRoutes(r chi.Router, d Deps) {
	if d.Matchmaker == nil {
		return
	}
	r.Route("/matchmaker/tickets", func(r chi.Router) {
		r.Use(tenant.RequireKeyScope(tenant.ScopeMatchmaker))
		r.Post("/", matchmakerCreateTicketHandler(d))
		r.Get("/{id}", matchmakerGetTicketHandler(d))
		r.Delete("/{id}", matchmakerCancelTicketHandler(d))
	})
}

func mountRelayRoutes(r chi.Router, d Deps) {
	if d.RelayIssuer == nil {
		return
	}
	r.With(tenant.RequireKeyScope(tenant.ScopeP2PRelay)).Post("/relay/credentials", relayCredentialsHandler(d))
}

func mountGameSessionRoutes(r chi.Router, d Deps) {
	r.Route("/game-session", func(r chi.Router) {
		r.Post("/", gameSessionCreateHandler(d))
		r.Get("/", gameSessionResolveHandler(d))
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", gameSessionGetHandler(d))
			r.Post("/join", gameSessionJoinHandler(d))
			r.Post("/heartbeat", gameSessionHeartbeatHandler(d))
			r.Delete("/", gameSessionLeaveHandler(d))
		})
	})
}
