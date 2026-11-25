package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

func getControlPanelAsset(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/assets/*", (&Handler{}).assetHandler)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestAssetHandler_should_send_immutable_when_version_matches(t *testing.T) {
	rec := getControlPanelAsset(t, "/assets/controlpanel.js?v="+controlPanelAssetVersions["controlpanel.js"])

	assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
}
