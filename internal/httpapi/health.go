package httpapi

import (
	"encoding/json"
	"net/http"
)

func healthzHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": d.Version,
			"commit":  d.Commit,
		})
	}
}
