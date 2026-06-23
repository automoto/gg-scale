package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/tenant"
)

func mountStorageRoutes(r chi.Router, d Deps) {
	r.Route("/storage", func(r chi.Router) {
		r.Get("/objects", storageListHandler(d))
		r.Put("/objects/{key}", storagePutHandler(d))
		r.Get("/objects/{key}", storageGetHandler(d))
		r.Delete("/objects/{key}", storageDeleteHandler(d))
	})
}

func mountLeaderboardRoutes(r chi.Router, d Deps) {
	r.Route("/leaderboards", func(r chi.Router) {
		// Score submission is server-authoritative: only callers with a
		// secret key (game server / tenant backend) may submit scores.
		// The end-user session in X-Session-Token still identifies the
		// subject — the secret key authorises the proxying caller.
		r.Group(func(r chi.Router) {
			r.Use(requireAPIKeyPermission(d, rbac.ObjectLeaderboard, rbac.ActionSubmit))
			r.Post("/{id}/scores", leaderboardSubmitHandler(d))
		})
		r.Get("/{id}/top", leaderboardTopHandler(d))
		r.Get("/{id}/around-me", leaderboardAroundMeHandler(d))
	})
}

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
		r.Post("/{user_id}/request", friendRequestHandler(d))
		r.Post("/{user_id}/accept", friendAcceptHandler(d))
		r.Post("/{user_id}/reject", friendRejectHandler(d))
		r.Delete("/{user_id}", friendDeleteHandler(d))
	})
}

func mountProfileRoutes(r chi.Router, d Deps) {
	r.Route("/profile", func(r chi.Router) {
		r.Get("/", profileGetHandler(d))
		r.Patch("/", profilePatchHandler(d))
	})
}

func mountRealtimeRoutes(r chi.Router, d Deps) {
	if d.Hub == nil {
		return
	}
	r.Get("/ws", realtime.ServeWS(realtime.Options{
		Hub:           d.Hub,
		Cache:         d.Cache,
		MaxPerTenant:  d.RealtimeMaxPerTenant,
		MaxPerEndUser: d.RealtimeMaxPerEndUser,
	}))
}

// mountFleetHeartbeatRoute is server-tier (secret api_key). Game-servers
// authenticate with their secret key and POST liveness + player count.
func mountFleetHeartbeatRoute(r chi.Router, d Deps) {
	if d.ServerList == nil {
		return
	}
	r.Post("/fleets/heartbeat", fleetHeartbeatHandler(d))
}

// mountFleetListRoute is end-user tier. Any authenticated session can
// browse the live server list for its tenant.
func mountFleetListRoute(r chi.Router, d Deps) {
	if d.ServerList == nil {
		return
	}
	r.Get("/fleets/{fleet}/servers", fleetServersListHandler(d))
}

func mountMatchmakerRoutes(r chi.Router, d Deps) {
	if d.Matchmaker == nil {
		return
	}
	r.Route("/matchmaker/tickets", func(r chi.Router) {
		r.Post("/", matchmakerCreateTicketHandler(d))
		r.Get("/{id}", matchmakerGetTicketHandler(d))
		r.Delete("/{id}", matchmakerCancelTicketHandler(d))
	})
}

func mountRelayRoutes(r chi.Router, d Deps) {
	if d.RelayIssuer == nil {
		return
	}
	r.Post("/relay/credentials", relayCredentialsHandler(d))
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

func mountPresenceRoutes(r chi.Router, d Deps) {
	r.Put("/presence", presenceUpdateHandler(d))
}

func mountGameInviteRoutes(r chi.Router, d Deps) {
	r.Route("/invite", func(r chi.Router) {
		r.Post("/", gameInviteCreateHandler(d))
		r.Get("/", gameInviteListHandler(d))
		r.Delete("/{id}", gameInviteDeleteHandler(d))
	})
}
