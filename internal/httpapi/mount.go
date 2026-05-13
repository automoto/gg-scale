package httpapi

import (
	"github.com/go-chi/chi/v5"

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
			r.Use(tenant.RequireKeyType(tenant.KeyTypeSecret))
			r.Post("/{id}/scores", leaderboardSubmitHandler(d))
		})
		r.Get("/{id}/top", leaderboardTopHandler(d))
		r.Get("/{id}/around-me", leaderboardAroundMeHandler(d))
	})
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
		Hub:          d.Hub,
		Cache:        d.Cache,
		MaxPerTenant: d.RealtimeMaxPerTenant,
	}))
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
