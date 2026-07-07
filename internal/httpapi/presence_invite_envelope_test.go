package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These lock in the wire contract: the migrated presence/invite handlers
// validate input shape in the schema (missing/blank required field → 422
// problem+json) and emit problem+json error bodies throughout. Both cases fail
// in huma's validation layer before any handler code runs, so Deps{} suffices.

func postEnvelope(t *testing.T, register func(api huma.API, d Deps), method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		register(groupAPI(r, newHumaConfig("test")), Deps{})
	})
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestPresence_empty_status_422_problem_json(t *testing.T) {
	rec := postEnvelope(t, registerPresence, http.MethodPut, "/v1/presence", `{"status":""}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
}

func TestPresence_oversize_status_422(t *testing.T) {
	rec := postEnvelope(t, registerPresence, http.MethodPut, "/v1/presence",
		`{"status":"`+strings.Repeat("x", 33)+`"}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
}

func TestGameInvite_missing_fields_422_problem_json(t *testing.T) {
	rec := postEnvelope(t, registerGameInvites, http.MethodPost, "/v1/invite", `{"to_email":"","session_id":""}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
}
