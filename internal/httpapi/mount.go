package httpapi

import "github.com/go-chi/chi/v5"

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
		r.Post("/{id}/scores", leaderboardSubmitHandler(d))
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
