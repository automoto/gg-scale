package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These exercise the shared huma config through a throwaway operation, proving
// the group-adapter binding and the error envelope the rest of the migration
// relies on: problem+json bodies, huma's additionalProperties rejection, the
// 1 MiB body cap, and JSON parse handling.

type humaSampleBody struct {
	Name string `json:"name"`
}

type humaSampleInput struct {
	Body humaSampleBody
}

type humaSampleOutput struct {
	Body humaSampleBody
}

func newHumaSampleServer(t *testing.T) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		api := groupAPI(r, newHumaConfig("test"))
		huma.Register(api, huma.Operation{
			OperationID: "sample",
			Method:      http.MethodPost,
			Path:        "/v1/sample",
		}, func(_ context.Context, in *humaSampleInput) (*humaSampleOutput, error) {
			return &humaSampleOutput{Body: in.Body}, nil
		})
	})
	return r
}

func postSample(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sample", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHuma_valid_body_echoes_200(t *testing.T) {
	rec := postSample(t, newHumaSampleServer(t), `{"name":"alice"}`)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"name":"alice"}`, rec.Body.String())
}

func TestHuma_unknown_field_rejected_422_problem_json(t *testing.T) {
	rec := postSample(t, newHumaSampleServer(t), `{"name":"alice","extra":1}`)

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
}

func TestHuma_malformed_json_rejected_400(t *testing.T) {
	rec := postSample(t, newHumaSampleServer(t), `{"name":`)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/problem+json")
}

func TestHuma_oversize_body_rejected_413(t *testing.T) {
	huge := `{"name":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := postSample(t, newHumaSampleServer(t), huge)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
}
