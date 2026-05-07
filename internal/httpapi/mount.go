package httpapi

import (
	"github.com/go-chi/chi/v5"

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

// mountFleetWriteRoutes wires /v1/fleet/servers writes called by game
// servers registering themselves. API-key auth via the surrounding tenant
// group; no session required.
func mountFleetWriteRoutes(r chi.Router, d Deps) {
	r.Route("/fleet/servers", func(r chi.Router) {
		r.Post("/", fleetRegisterHandler(d))
		r.Put("/{id}/heartbeat", fleetHeartbeatHandler(d))
		r.Delete("/{id}", fleetDeregisterHandler(d))
	})
}

// mountFleetReadRoutes wires /v1/fleet/servers reads called by clients
// browsing for servers. End-user session auth via the surrounding enduser
// group.
func mountFleetReadRoutes(r chi.Router, d Deps) {
	r.Get("/fleet/servers", fleetListHandler(d))
}
